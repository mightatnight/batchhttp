package batchhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// Client is the HTTP client used for all requests. If nil, http.DefaultClient is used.
	//
	// Production note: you typically want to set a Timeout and a Transport with sane limits.
	Client *http.Client

	// Concurrency is the maximum number of in-flight requests for RunBatch/RunRepeated.
	// If <= 0, defaults to 1.
	Concurrency int

	// Attempts is the maximum attempts per request (1 = no retries).
	// If <= 0, defaults to 1.
	Attempts int

	// Retry configures retry behavior. If nil, default behavior is:
	//   - Attempts respected, but no retries unless Attempts > 1 AND error is retryable.
	//   - Retryable errors include temporary network errors and some 5xx responses (via DefaultRetryPolicy).
	Retry RetryPolicy

	// DrainBody, when true, drains and closes response bodies automatically inside Runner.Do/Run*.
	//
	// This is recommended for production when you care about connection reuse and are not
	// reading the body elsewhere. If you need the body, set DrainBody=false and close yourself.
	DrainBody bool

	// DrainLimit is the maximum number of bytes to drain when DrainBody is true.
	// If <= 0, defaults to 1 MiB.
	DrainLimit int64

	// Now allows tests to inject a clock. If nil, time.Now is used.
	Now func() time.Time
}

// Runner executes HTTP requests with timing, retries and concurrency controls.
type Runner struct {
	client      *http.Client
	concurrency int
	attempts    int
	retry       RetryPolicy
	drainBody   bool
	drainLimit  int64
	now         func() time.Time
}

// NewRunner constructs a Runner from the provided config.
func NewRunner(cfg RunnerConfig) *Runner {
	c := cfg.Client
	if c == nil {
		c = http.DefaultClient
	}

	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 1
	}

	attempts := cfg.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	drainLimit := cfg.DrainLimit
	if drainLimit <= 0 {
		drainLimit = 1 << 20 // 1MiB
	}

	retry := cfg.Retry
	if retry == nil {
		retry = DefaultRetryPolicy{}
	}

	return &Runner{
		client:      c,
		concurrency: conc,
		attempts:    attempts,
		retry:       retry,
		drainBody:   cfg.DrainBody,
		drainLimit:  drainLimit,
		now:         now,
	}
}

// Do performs a single HTTP request with the Runner's retry policy.
//
// It returns a ManagedResponse which includes the underlying *http.Response plus
// metadata about attempts and timing.
//
// If r.drainBody is true, Do will not drain/close the response automatically, because the
// caller might need to read it. Use ManagedResponse.Close() to drain+close when done.
//
// Note: If you need to ensure the body is always drained/closed regardless of caller usage,
// prefer using RunBatch/RunRepeated which will apply DrainBody automatically for you.
func (r *Runner) Do(ctx context.Context, req *http.Request) (*ManagedResponse, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}

	// Ensure context is attached. If req already has a ctx, Clone replaces it.
	req = req.Clone(ctx)

	start := r.now()
	var lastErr error
	var lastRes *http.Response

	for attempt := 1; attempt <= r.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		attemptStart := r.now()
		res, err := r.client.Do(req)
		attemptDur := r.now().Sub(attemptStart)

		if err == nil && res == nil {
			err = errors.New("http client returned nil response without error")
		}

		if err == nil {
			lastRes = res
			lastErr = nil
		} else {
			lastRes = nil
			lastErr = err
		}

		if attempt == r.attempts {
			break
		}

		decision := r.retry.Decide(RetryInput{
			Attempt: attempt,
			Elapsed: r.now().Sub(start),
			Request: req,
			Response: func() *http.Response {
				// If we got a response, pass it to policy.
				return res
			}(),
			Err: err,
			// Duration of this attempt can help policies decide.
			AttemptDuration: attemptDur,
		})

		if !decision.Retry {
			break
		}

		// If we got a response but will retry, ensure body is consumed/closed;
		// otherwise connections may be leaked and keep-alives disabled.
		if res != nil && res.Body != nil {
			_ = drainAndClose(res.Body, r.drainLimit)
		}

		if decision.Backoff > 0 {
			timer := time.NewTimer(decision.Backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}

	total := r.now().Sub(start)

	mr := &ManagedResponse{
		Response: lastRes,
		Request:  req,
		Attempts: r.attempts,
		Duration: total,
		Err:      lastErr,
		// Close uses runner drain configuraration so the caller doesn't need to.
		drainLimit: r.drainLimit,
		drainBody:  r.drainBody,
	}

	// If we failed, ensure we didn't leak any body on a non-nil response (shouldn't happen
	// because lastRes is only set on err==nil), but be defensive.
	if lastErr != nil && lastRes != nil && lastRes.Body != nil {
		_ = drainAndClose(lastRes.Body, r.drainLimit)
	}

	if lastErr != nil {
		return mr, lastErr
	}
	return mr, nil
}

// ManagedResponse wraps an *http.Response and includes metadata.
// Use Close() to drain/close if you enabled DrainBody, or just Close() anyway as best practice.
type ManagedResponse struct {
	*http.Response

	Request  *http.Request
	Attempts int
	Duration time.Duration
	Err      error

	drainLimit int64
	drainBody  bool

	closeOnce sync.Once
	closeErr  error
}

// Close drains (optional) and closes the underlying response body.
func (mr *ManagedResponse) Close() error {
	mr.closeOnce.Do(func() {
		if mr.Response == nil || mr.Body == nil {
			return
		}
		if mr.drainBody {
			mr.closeErr = drainAndClose(mr.Body, mr.drainLimit)
			return
		}
		mr.closeErr = mr.Body.Close()
	})
	return mr.closeErr
}

// RepeatSpec defines how to build repeated requests for RunRepeated.
type RepeatSpec struct {
	// Count is the number of runs.
	// If <= 0, RunRepeated returns empty results.
	Count int

	// Label is an optional label copied into each result for grouping.
	Label string

	// Build constructs a fresh request for each run.
	// It is called with run index i from 1..Count (inclusive).
	//
	// Important: Build must return a request whose Body can be read once by the client.
	// If you need retries for requests with bodies, ensure the Request.GetBody is set,
	// so the runner can safely rebuild the body. Otherwise, retries of a POST/PUT body
	// may be unsafe or will be disabled by the retry policy.
	Build func(i int) (*http.Request, error)
}

// BatchSpec defines a batch of independent requests for RunBatch.
type BatchSpec struct {
	// Label is an optional label copied into each result.
	Label string

	// Requests are the requests to execute (each at most once, but with retries per RunnerConfig.Attempts).
	//
	// Note: requests with bodies should be unique per element; do not reuse the same *http.Request
	// across multiple entries unless you know what you're doing.
	Requests []*http.Request
}

// RunResult captures a single completed run (one logical request run, potentially multiple attempts with retries).
type RunResult struct {
	Label string

	// Run is 1-based when using RunRepeated. For RunBatch, Run is 0.
	Run int

	// Index is 0-based index within the batch (for RunBatch). For RunRepeated, Index is Run-1.
	Index int

	Method string
	URL    string

	Attempted int

	StatusCode int
	Duration   time.Duration

	// Error is the terminal error for this run (context cancellation, network errors, etc.).
	// If Err is nil, StatusCode will be non-zero.
	Err error
}

// Summary provides aggregate stats.
type Summary struct {
	Label string

	Total   int
	Success int
	Failed  int

	SuccessRate float64

	// Timing stats computed over successful runs only (Duration), unless noted otherwise.
	Avg time.Duration
	P50 time.Duration
	P90 time.Duration
	P99 time.Duration
	Min time.Duration
	Max time.Duration

	// TotalDuration is wall-clock time from start to end of the entire call (includes concurrency effects).
	TotalDuration time.Duration
}

// RunRepeated runs the request built by spec.Build spec.Count times.
// Concurrency controls how many runs are in-flight.
func (r *Runner) RunRepeated(ctx context.Context, spec RepeatSpec) ([]RunResult, Summary) {
	start := r.now()
	if spec.Count <= 0 {
		return nil, Summary{
			Label:         spec.Label,
			Total:         0,
			TotalDuration: r.now().Sub(start),
		}
	}
	if spec.Build == nil {
		results := make([]RunResult, 0, spec.Count)
		for i := 1; i <= spec.Count; i++ {
			results = append(results, RunResult{
				Label: spec.Label,
				Run:   i,
				Index: i - 1,
				Err:   errors.New("nil Build function"),
			})
		}
		s := summarize(spec.Label, results, r.now().Sub(start))
		return results, s
	}

	type job struct {
		i int
	}
	jobs := make(chan job)
	results := make([]RunResult, spec.Count)

	var wg sync.WaitGroup
	workers := r.concurrency
	if workers > spec.Count {
		workers = spec.Count
	}
	if workers <= 0 {
		workers = 1
	}

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				run := j.i
				idx := run - 1

				req, err := spec.Build(run)
				if err != nil {
					results[idx] = RunResult{
						Label: spec.Label,
						Run:   run,
						Index: idx,
						Err:   err,
					}
					continue
				}
				if req == nil {
					results[idx] = RunResult{
						Label: spec.Label,
						Run:   run,
						Index: idx,
						Err:   errors.New("Build returned nil request"),
					}
					continue
				}

				rr := r.executeOne(ctx, req, spec.Label, run, idx)

				// Automatic draining/closing if configured AND response exists:
				// executeOne returns only metadata, so we ensure it doesn't leak bodies by draining
				// inside executeOne (via ManagedResponse.Close()).
				results[idx] = rr
			}
		}()
	}

	for i := 1; i <= spec.Count; i++ {
		select {
		case <-ctx.Done():
			// Stop scheduling new jobs; workers will finish current ones.
			close(jobs)
			wg.Wait()
			totalDur := r.now().Sub(start)
			s := summarize(spec.Label, results, totalDur)
			return results, s
		case jobs <- job{i: i}:
		}
	}

	close(jobs)
	wg.Wait()

	totalDur := r.now().Sub(start)
	s := summarize(spec.Label, results, totalDur)
	return results, s
}

// RunBatch runs all requests in spec.Requests with concurrency controls.
func (r *Runner) RunBatch(ctx context.Context, spec BatchSpec) ([]RunResult, Summary) {
	start := r.now()
	n := len(spec.Requests)
	if n == 0 {
		return nil, Summary{
			Label:         spec.Label,
			Total:         0,
			TotalDuration: r.now().Sub(start),
		}
	}

	type job struct {
		idx int
	}
	jobs := make(chan job)
	results := make([]RunResult, n)

	var wg sync.WaitGroup
	workers := r.concurrency
	if workers > n {
		workers = n
	}
	if workers <= 0 {
		workers = 1
	}

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				idx := j.idx
				req := spec.Requests[idx]
				if req == nil {
					results[idx] = RunResult{
						Label: spec.Label,
						Run:   0,
						Index: idx,
						Err:   errors.New("nil request"),
					}
					continue
				}
				results[idx] = r.executeOne(ctx, req, spec.Label, 0, idx)
			}
		}()
	}

	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			totalDur := r.now().Sub(start)
			s := summarize(spec.Label, results, totalDur)
			return results, s
		case jobs <- job{idx: i}:
		}
	}

	close(jobs)
	wg.Wait()

	totalDur := r.now().Sub(start)
	s := summarize(spec.Label, results, totalDur)
	return results, s
}

func (r *Runner) executeOne(ctx context.Context, req *http.Request, label string, run int, idx int) RunResult {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	urlStr := ""
	if req.URL != nil {
		urlStr = req.URL.String()
	}

	// If the request contains a body and we intend to retry, we need it to be replayable.
	// net/http supports this via Request.GetBody. We don't rewire the request here; instead
	// we let the retry policy avoid retrying non-replayable bodies (DefaultRetryPolicy does).
	mr, err := r.Do(ctx, req)

	rr := RunResult{
		Label:     label,
		Run:       run,
		Index:     idx,
		Method:    method,
		URL:       urlStr,
		Attempted: r.attempts,
		Duration:  0,
		Err:       nil,
	}

	if mr != nil {
		rr.Duration = mr.Duration
	}

	if err != nil {
		rr.Err = err
		// Ensure any response body is closed/drained if we got one.
		if mr != nil {
			_ = mr.Close()
			if mr.Response != nil {
				rr.StatusCode = mr.StatusCode
			}
		}
		return rr
	}

	if mr == nil || mr.Response == nil {
		rr.Err = errors.New("nil response without error")
		return rr
	}

	rr.StatusCode = mr.StatusCode

	// Drain/close on success if configured.
	_ = mr.Close()

	return rr
}

// RetryPolicy decides whether to retry after an attempt.
type RetryPolicy interface {
	Decide(in RetryInput) RetryDecision
}

// RetryInput contains attempt details for retry decisions.
type RetryInput struct {
	Attempt         int
	Elapsed         time.Duration
	AttemptDuration time.Duration
	Request         *http.Request
	Response        *http.Response
	Err             error
}

// RetryDecision is the output of a retry policy.
type RetryDecision struct {
	Retry   bool
	Backoff time.Duration
	Reason  string
}

// DefaultRetryPolicy provides safe retries:
//   - Retries only on retryable network errors or selected 5xx.
//
// //  - Does NOT retry non-idempotent methods with non-replayable bodies (GetBody==nil).
//   - Exponential backoff with jitterless cap (simple, predictable).
type DefaultRetryPolicy struct {
	// RetryOn5xx enables retries for 5xx responses (except 501 by default).
	// If nil, defaults to true.
	RetryOn5xx *bool

	// MaxBackoff caps exponential backoff. If 0, defaults to 2s.
	MaxBackoff time.Duration

	// BaseBackoff is initial backoff before jitter. If 0, defaults to 100ms.
	BaseBackoff time.Duration
}

func (p DefaultRetryPolicy) Decide(in RetryInput) RetryDecision {
	// No retries if we were successful.
	if in.Err == nil && in.Response != nil && in.Response.StatusCode < 500 {
		return RetryDecision{Retry: false, Reason: "success"}
	}

	req := in.Request
	method := ""
	if req != nil {
		method = req.Method
	}

	// If request has a non-nil body, ensure it's replayable for retries.
	// If GetBody is nil, net/http - and we - can't safely replay it.
	if req != nil && req.Body != nil && req.GetBody == nil {
		// Allow "safe" methods with no body; but Body!=nil means something was provided.
		// Being conservative: do not retry.
		return RetryDecision{Retry: false, Reason: "non-replayable request body"}
	}

	// Decide retryability.
	if in.Err != nil {
		if !isRetryableNetErr(in.Err) {
			return RetryDecision{Retry: false, Reason: "non-retryable error"}
		}
		return RetryDecision{
			Retry:   true,
			Backoff: p.backoff(in.Attempt),
			Reason:  "retryable network error",
		}
	}

	// Retry on selected 5xx.
	retry5xx := true
	if p.RetryOn5xx != nil {
		retry5xx = *p.RetryOn5xx
	}
	if retry5xx && in.Response != nil {
		code := in.Response.StatusCode
		if code >= 500 && code <= 599 && code != http.StatusNotImplemented {
			// For non-idempotent methods, you may want to avoid retrying even on 5xx.
			// We keep it conservative: allow retry only for safe/idempotent methods.
			if !isIdempotentMethod(method) {
				return RetryDecision{Retry: false, Reason: "non-idempotent method on 5xx"}
			}
			return RetryDecision{
				Retry:   true,
				Backoff: p.backoff(in.Attempt),
				Reason:  fmt.Sprintf("server %d", code),
			}
		}
	}

	return RetryDecision{Retry: false, Reason: "no retry condition met"}
}

func (p DefaultRetryPolicy) backoff(attempt int) time.Duration {
	base := p.BaseBackoff
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	maxB := p.MaxBackoff
	if maxB <= 0 {
		maxB = 2 * time.Second
	}

	// attempt is 1-based. Backoff for retry before attempt+1.
	exp := float64(attempt - 1)
	if exp < 0 {
		exp = 0
	}
	d := time.Duration(float64(base) * math.Pow(2, exp))
	if d > maxB {
		d = maxB
	}
	return d
}

func isIdempotentMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}

	// context cancellation/timeouts should not be retried here.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// net.Error can indicate temporary/timeouts.
	var ne net.Error
	if errors.As(err, &ne) {
		// timeouts are often not useful to retry immediately, but in load testing
		// you may want to. Default: allow temporary; allow timeout as retryable too.
		if ne.Temporary() || ne.Timeout() {
			return true
		}
	}

	// Some errors are wrapped; treat EOF and connection resets as retryable.
	// We keep this minimal to avoid "lying" about unknown errors.
	if errors.Is(err, io.EOF) {
		return true
	}

	// Common syscall-ish network errors can be nested; net.OpError often wraps them.
	var oe *net.OpError
	if errors.As(err, &oe) {
		// If it's a network op error, many are transient.
		return true
	}

	return false
}

func drainAndClose(rc io.ReadCloser, limit int64) error {
	if rc == nil {
		return nil
	}
	_, _ = io.CopyN(io.Discard, rc, limit)
	return rc.Close()
}

func summarize(label string, results []RunResult, total time.Duration) Summary {
	totalRuns := len(results)
	success := 0
	fail := 0
	var durs []time.Duration

	for _, r := range results {
		if r.Err == nil && r.StatusCode != 0 && r.StatusCode < 500 {
			success++
			durs = append(durs, r.Duration)
		} else if r.Err != nil || r.StatusCode != 0 {
			// Count as failed if we have any signal; keep nil entries as neither.
			fail++
		}
	}

	s := Summary{
		Label:         label,
		Total:         totalRuns,
		Success:       success,
		Failed:        fail,
		TotalDuration: total,
	}

	if totalRuns > 0 {
		s.SuccessRate = float64(success) / float64(totalRuns)
	}

	if len(durs) == 0 {
		return s
	}

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })

	s.Min = durs[0]
	s.Max = durs[len(durs)-1]
	s.Avg = avgDuration(durs)
	s.P50 = percentileDuration(durs, 0.50)
	s.P90 = percentileDuration(durs, 0.90)
	s.P99 = percentileDuration(durs, 0.99)

	return s
}

func avgDuration(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	var sum int64
	for _, d := range durs {
		sum += int64(d)
	}
	return time.Duration(sum / int64(len(durs)))
}

func percentileDuration(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	// Nearest-rank style with interpolation-friendly index.
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	// Linear interpolation in nanoseconds.
	loN := float64(sorted[lo])
	hiN := float64(sorted[hi])
	return time.Duration(loN + (hiN-loN)*frac)
}
