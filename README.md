# test-suite

A small Go module for running batched HTTP requests with concurrency limits, per-run timing, retries, and summary statistics.

The core package is:

- `test-suite/batchhttp`

It is designed to be production-friendly:
- accepts standard `net/http` requests (`*http.Request`)
- lets you plug in your own `*http.Client` (timeouts, transport, proxy, mTLS, etc.)
- supports concurrency-capped batch execution
- returns structured results per run (status, duration, error)
- supports safe default retry behavior (retryable network errors, selected 5xx with idempotency rules)

---

## Install / Import

From another Go module:

```/dev/null/install.txt#L1-3
go get <your-repo-host>/test-suite@latest
```

Then import:

```/dev/null/import.go#L1-8
import (
  "test-suite/batchhttp"
)
```

> Note: your `go.mod` in this repo currently declares `module test-suite`, so local imports look like `test-suite/batchhttp`.
> If you publish this module, you’ll usually change that module path to your VCS URL (e.g. `github.com/you/test-suite`).

---

## Package overview (`batchhttp`)

### What you get

- `batchhttp.Runner` created via `batchhttp.NewRunner(batchhttp.RunnerConfig{...})`
- `Runner.Do(ctx, req)` for single request execution (with retry policy)
- `Runner.RunRepeated(ctx, batchhttp.RepeatSpec{...})` for “run the same thing N times”
- `Runner.RunBatch(ctx, batchhttp.BatchSpec{...})` for “run these N requests”
- `batchhttp.Summary` with aggregate stats (success rate, avg/min/max, p50/p90/p99 for successful runs)
- Optional human-readable formatting helpers in `batchhttp/format.go`

---

## Quick start: repeat a GET request 10 times

```/dev/null/quickstart.go#L1-66
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"test-suite/batchhttp"
)

func main() {
	client := &http.Client{Timeout: 10 * time.Second}

	runner := batchhttp.NewRunner(batchhttp.RunnerConfig{
		Client:      client,
		Concurrency: 4,
		Attempts:    1,
		DrainBody:   true, // recommended if you don't read bodies
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseReq, _ := http.NewRequest(http.MethodGet, "https://example.com/health", nil)
	baseReq.Header.Set("accept", "application/json")

	results, summary := runner.RunRepeated(ctx, batchhttp.RepeatSpec{
		Count: 10,
		Label: "health",
		Build: func(i int) (*http.Request, error) {
			// Clone attaches the ctx and ensures it’s safe to reuse headers across runs.
			return baseReq.Clone(ctx), nil
		},
	})

	fmt.Printf("success rate: %.1f%% (ok=%d fail=%d)\n", summary.SuccessRate*100, summary.Success, summary.Failed)
	for _, r := range results {
		fmt.Println(r.Run, r.StatusCode, r.Duration, r.Err)
	}
}
```

---

## Workflow example: token then lookup (two-step flow)

This mirrors the common “get token then call API” pattern. The important part is: **you build a fresh request per run**.

```/dev/null/workflow.go#L1-140
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"test-suite/batchhttp"
)

type tokenResp struct {
	Data struct {
		Token struct {
			Hash string `json:"hash"`
		} `json:"token"`
	} `json:"data"`
}

func main() {
	client := &http.Client{Timeout: 20 * time.Second}

	runner := batchhttp.NewRunner(batchhttp.RunnerConfig{
		Client:      client,
		Concurrency: 1,    // sequential runs (often desired for reliability tests)
		Attempts:    2,    // allow retries where policy permits
		DrainBody:   true, // keep connections reusable
	})

	confirmationNumber := "FF1LJ9"
	lastName := "Hollander"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	results, summary := runner.RunRepeated(ctx, batchhttp.RepeatSpec{
		Count: 10,
		Label: "trip-lookup",
		Build: func(i int) (*http.Request, error) {
			// 1) Token request
			tokenReq, _ := http.NewRequest(http.MethodGet, "https://www.united.com/api/auth/anonymous-token", nil)
			tokenReq.Header.Set("accept", "application/json")

			tokenRes, err := runner.Do(ctx, tokenReq)
			if err != nil {
				return nil, err
			}
			defer tokenRes.Close()

			var tr tokenResp
			if err := json.NewDecoder(tokenRes.Body).Decode(&tr); err != nil {
				return nil, err
			}

			// 2) Lookup request
			payload := map[string]any{
				"confirmationNumber":   confirmationNumber,
				"lastName":             lastName,
				"mpUserName":           "",
				"region":               "US",
				"isDecryptNeeded":      true,
				"encryptedTripDetails": "",
				"IsIncludeUCDData":     true,
				"IsIncludeIrrops":      true,
				"IsPMDRequired":        true,
			}
			b, _ := json.Marshal(payload)

			lookupReq, _ := http.NewRequest(http.MethodPost, "https://www.united.com/api/myTrips/lookup", bytes.NewReader(b))
			lookupReq.Header.Set("content-type", "application/json")
			lookupReq.Header.Set("accept", "application/json")
			lookupReq.Header.Set("x-authorization-api", "bearer "+tr.Data.Token.Hash)

			return lookupReq, nil
		},
	})

	fmt.Printf("runs=%d ok=%d fail=%d success=%.1f%%\n", summary.Total, summary.Success, summary.Failed, summary.SuccessRate*100)
	for _, r := range results {
		fmt.Printf("run=%d status=%d dur=%s err=%v\n", r.Run, r.StatusCode, r.Duration, r.Err)
	}
}
```

---

## Using your own `http.Client` (recommended)

You should usually provide a custom `*http.Client` in production:
- set `Timeout`
- configure a tuned `Transport` for concurrency/keep-alive behavior
- configure proxies, TLS, mTLS, etc.

Example transport tuning:

```/dev/null/client.go#L1-45
transport := &http.Transport{
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 50,
	IdleConnTimeout:     90 * time.Second,
	// TLSClientConfig: ...
	// Proxy: http.ProxyFromEnvironment,
}

client := &http.Client{
	Timeout:   15 * time.Second,
	Transport: transport,
}
```

Then:

```/dev/null/runner.go#L1-12
runner := batchhttp.NewRunner(batchhttp.RunnerConfig{
	Client:      client,
	Concurrency: 10,
	Attempts:    2,
	DrainBody:   true,
})
```

---

## Retries: what’s safe by default

The built-in `DefaultRetryPolicy` is conservative:
- retries retryable network errors
- can retry certain 5xx responses
- avoids retrying **non-idempotent methods** on 5xx
- avoids retrying requests with **non-replayable bodies** (`req.Body != nil` and `req.GetBody == nil`)

### Replayable request bodies

If you want retries for `POST`/`PUT` with a body, ensure `req.GetBody` is set. Some request constructors (or your own code) can set it.

If you build your body from `[]byte`, you can do:

```/dev/null/getbody.go#L1-28
body := []byte(`{"hello":"world"}`)
req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
req.GetBody = func() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(body)), nil
}
```

---

## Draining response bodies and connection reuse

If you don’t read the response body, you still need to close it.
To keep keep-alives effective, draining the body is often important.

- Set `RunnerConfig.DrainBody = true` to drain+close automatically inside batch execution.
- Or explicitly `defer resp.Body.Close()` and read/discard content yourself.

---

## Human-readable output (optional)

If you want simple CLI-friendly output:

```/dev/null/format.go#L1-35
opt := batchhttp.DefaultFormatOptions()
batchhttp.WriteText(os.Stdout, results, summary, opt)
```

---

## Notes / Limitations

- `RunRepeated` expects `RepeatSpec.Build` to return a **fresh** request for each run.
- For high throughput tests, tune your HTTP transport and use `DrainBody=true`.
- Success in `Summary` is currently counted as: `Err == nil` and `StatusCode < 500`.
  Adjust your own interpretation by post-processing `[]RunResult` if you need stricter rules.

---

## Directory layout

- `batchhttp/` — main package implementation (`Runner`, retry policy, formatting)

---
