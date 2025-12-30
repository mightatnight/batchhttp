// Package batchhttp provides a small, production-ready toolkit for running
// batched HTTP requests with concurrency limits, per-request timing, retries,
// and optional per-run summary statistics.
//
// Design goals:
//   - Accept standard net/http requests (http.Request).
//   - Allow callers to supply their own *http.Client (timeouts, transports, proxies, mTLS, etc.).
//   - Support controlled concurrency and deterministic iteration counts (similar to a "batch test").
//   - Return structured per-run results with timing and error details.
//   - Be safe for production use (context-aware, no global state, no hidden goroutines).
//
// Typical use cases:
//   - Reliability testing: run the same HTTP workflow N times and measure latency distribution.
//   - Load probing: run many independent requests with a concurrency cap.
//   - Regression testing: capture response codes and timing changes over time.
//
// Quick start (single request, repeated N times):
//
// ```/dev/null/example.go#L1-60
// package main
//
// import (
//
//	"context"
//	"fmt"
//	"net/http"
//	"time"
//
//	"test-suite/batchhttp"
//
// )
//
//	func main() {
//		client := &http.Client{Timeout: 15 * time.Second}
//
//		req, _ := http.NewRequest(http.MethodGet, "https://example.com/health", nil)
//		req.Header.Set("accept", "application/json")
//
//		runner := batchhttp.NewRunner(batchhttp.RunnerConfig{
//			Client:      client, // optional; defaults to http.DefaultClient
//			Concurrency: 4,      // optional; defaults to 1
//			Attempts:    1,      // optional; defaults to 1
//			Retry:       nil,    // optional
//			DrainBody:   true,   // recommended when reusing connections
//		})
//
//		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//		defer cancel()
//
//		results, summary := runner.RunRepeated(ctx, batchhttp.RepeatSpec{
//			Count:  10,
//			Build:  func(i int) (*http.Request, error) { return req.Clone(ctx), nil },
//			Label:  "health-check",
//		})
//
//		fmt.Println("success rate:", summary.SuccessRate)
//		for _, r := range results {
//			fmt.Println(r.Run, r.StatusCode, r.Duration, r.Err)
//		}
//	}
//
// ```
//
// Workflow example (token then lookup), modeled after a "two-step" API flow:
//
// ```/dev/null/workflow.go#L1-120
// package main
//
// import (
//
//	"bytes"
//	"context"
//	"encoding/json"
//	"fmt"
//	"net/http"
//	"time"
//
//	"test-suite/batchhttp"
//
// )
//
//	type tokenResp struct {
//		Data struct {
//			Token struct {
//				Hash string `json:"hash"`
//			} `json:"token"`
//		} `json:"data"`
//	}
//
//	func main() {
//		client := &http.Client{Timeout: 20 * time.Second}
//		runner := batchhttp.NewRunner(batchhttp.RunnerConfig{Client: client, Concurrency: 1, DrainBody: true})
//
//		confirmationNumber := "FF1LJ9"
//		lastName := "Hollander"
//
//		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
//		defer cancel()
//
//		results, summary := runner.RunRepeated(ctx, batchhttp.RepeatSpec{
//			Count: 10,
//			Label: "trip-lookup",
//			Build: func(i int) (*http.Request, error) {
//				// 1) Token request
//				tokenReq, _ := http.NewRequest(http.MethodGet, "https://www.united.com/api/auth/anonymous-token", nil)
//				tokenReq.Header.Set("accept", "application/json")
//
//				tokenRes, err := runner.Do(ctx, tokenReq)
//				if err != nil {
//					return nil, err
//				}
//				defer tokenRes.Close()
//
//				var tr tokenResp
//				if err := json.NewDecoder(tokenRes.Body).Decode(&tr); err != nil {
//					return nil, err
//				}
//
//				// 2) Lookup request
//				payload := map[string]any{
//					"confirmationNumber":   confirmationNumber,
//					"lastName":             lastName,
//					"mpUserName":           "",
//					"region":               "US",
//					"isDecryptNeeded":      true,
//					"encryptedTripDetails": "",
//					"IsIncludeUCDData":     true,
//					"IsIncludeIrrops":      true,
//					"IsPMDRequired":        true,
//				}
//				b, _ := json.Marshal(payload)
//
//				lookupReq, _ := http.NewRequest(http.MethodPost, "https://www.united.com/api/myTrips/lookup", bytes.NewReader(b))
//				lookupReq.Header.Set("content-type", "application/json")
//				lookupReq.Header.Set("accept", "application/json")
//				lookupReq.Header.Set("x-authorization-api", "bearer "+tr.Data.Token.Hash)
//
//				return lookupReq, nil
//			},
//		})
//
//		fmt.Println("runs:", summary.Total, "ok:", summary.Success, "fail:", summary.Failed)
//		for _, r := range results {
//			fmt.Printf("run=%d status=%d dur=%s err=%v\n", r.Run, r.StatusCode, r.Duration, r.Err)
//		}
//	}
//
// ```
//
// Notes:
//   - Requests are *not* reusable across attempts/runs unless you rebuild or clone them.
//     Provide a builder function (RepeatSpec.Build) that creates a fresh request each time.
//   - For connection reuse, draining/closing response bodies is important; set DrainBody=true
//     and/or ensure you always Close() response bodies yourself.
package batchhttp
