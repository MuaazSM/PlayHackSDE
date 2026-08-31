// Command load fires the 6 PM surge at a running API and checks the two latency
// budgets CLAUDE.md commits to.
//
//	make load                 # 500 concurrent bookers, one slot
//	go run ./test/load -n 500 -json
//
// It drives REAL HTTP through the REAL middleware chain — rate limiting,
// idempotency, shedding, timeout — because the p99s being asserted are p99s a
// student experiences, and a driver that called the booking service directly
// would be measuring a system nobody uses.
//
// By default it starts the API in-process on a loopback port and stops it again.
// That is deliberate: it means `make load` has no host dependency beyond the
// Postgres the rest of the project already needs, and no k6, no vegeta, and no
// venue wifi can stand between the demo and its numbers.
//
// -url points it at an already-running server instead. Two things change and
// both will surprise you once: every virtual user shares one source address, so
// RATE_LIMIT_IP_PER_MIN (600 by default) starts answering 429 after the first
// run and the second run measures the rate limiter; and WRITE_QUEUE_DEPTH is
// whatever that server was started with, not what this process sees. The
// in-process mode is the one to quote numbers from.
//
// Thresholds are FAILING conditions, not report lines. Exit status is 1 when any
// of them is missed:
//
//	p99 of 409 responses < 150 ms    losing must be faster than winning
//	p99 of 201 responses < 250 ms
//	zero 5xx                         a 500 is the system not knowing what happened
//
// The correctness invariant is checked too — exactly one 201 for one exclusive
// slot — because a run where every request failed would satisfy all three
// latency thresholds beautifully.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Budgets are the thresholds from CLAUDE.md's non-negotiable #6. Losing is the
// majority experience at peak, so the loser's budget is the tighter one.
const (
	ConflictP99Budget  = 150 * time.Millisecond
	ConfirmedP99Budget = 250 * time.Millisecond
)

// Attempt is one booking request's outcome.
type Attempt struct {
	Status  int
	Latency time.Duration
	Err     error
}

// Report is what one run measured.
type Report struct {
	N           int            `json:"n"`
	Wall        time.Duration  `json:"wall_ns"`
	ByStatus    map[int]int    `json:"by_status"`
	Errors      int            `json:"transport_errors"`
	ConfirmP99  time.Duration  `json:"confirmed_p99_ns"`
	ConflictP99 time.Duration  `json:"conflict_p99_ns"`
	ShedP99     time.Duration  `json:"shed_p99_ns"`
	Failures    []string       `json:"failures"`
}

// Confirmed counts 201s. A 200 is an idempotent replay, which cannot happen here
// — every request carries its own key — so it is counted separately and its
// presence would itself be a finding.
func (r *Report) Confirmed() int { return r.ByStatus[http.StatusCreated] }

// Conflicts counts 409s: the exclusion constraint, or a full capacity counter.
func (r *Report) Conflicts() int { return r.ByStatus[http.StatusConflict] }

// Shed counts 429s: the write queue bound, or the rate limiter.
func (r *Report) Shed() int { return r.ByStatus[http.StatusTooManyRequests] }

// ServerErrors counts 5xx. Must be zero.
func (r *Report) ServerErrors() int {
	n := 0
	for status, count := range r.ByStatus {
		if status >= 500 {
			n += count
		}
	}
	return n
}

// Runner fires n concurrent booking requests at one facility and one slot.
type Runner struct {
	Client     *http.Client
	BaseURL    string
	FacilityID uuid.UUID
	Start      time.Time
	Minutes    int

	// Tokens is one bearer per virtual user. Distinct users on purpose: five
	// hundred requests from one account would be rejected by the fair-use cap
	// and the idempotency index rather than by the exclusion constraint, and the
	// run would measure the wrong rejection.
	Tokens []string
}

// newHTTPClient keeps the load driver from turning transient loopback listener
// pressure into lost requests. It does not cap request concurrency: every
// caller still enters Runner.Run together, and only a failed TCP dial is
// retried before its error is reported.
func newHTTPClient(n int, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = n + 16
	transport.MaxIdleConnsPerHost = n + 16
	transport.MaxConnsPerHost = n + 16
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialWithRetry(dialer)
	return &http.Client{Timeout: timeout, Transport: transport}
}

func dialWithRetry(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		var lastErr error
		for attempt := 0; attempt < 4; attempt++ {
			conn, err := dialer.DialContext(ctx, network, address)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if attempt == 3 {
				break
			}
			timer := time.NewTimer(time.Duration(1<<uint(attempt)) * 5 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
}

// Run releases every goroutine at once and collects the split.
//
// The barrier matters. Goroutines started in a loop arrive spread over however
// long the loop took, which at n=500 is long enough that the first request can
// commit before the last one is built — and a race nobody was in is not a race.
//
// The connections are warmed BEFORE the barrier, in waves of 25. Releasing n
// goroutines against a cold pool means n simultaneous loopback dials, and on
// Windows a listener answers part of that SYN burst with a refusal. A refused
// dial is a transport error about the test machine, not a measurement of the
// write path — and it used to fail the majority of runs here.
func (r *Runner) Run(ctx context.Context, n int) (*Report, error) {
	if err := r.warmConnections(ctx, n); err != nil {
		return nil, err
	}

	var (
		wg       sync.WaitGroup
		barrier  = make(chan struct{})
		attempts = make([]Attempt, n)
	)

	body := func() []byte {
		raw, _ := json.Marshal(map[string]any{
			"facility_id":      r.FacilityID.String(),
			"start":            r.Start.UTC().Format(time.RFC3339),
			"duration_minutes": r.Minutes,
		})
		return raw
	}()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			attempts[i] = r.fire(ctx, body, r.Tokens[i%len(r.Tokens)])
		}(i)
	}

	// Give every goroutine a moment to reach the barrier before opening it.
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	close(barrier)
	wg.Wait()
	wall := time.Since(started)

	return summarise(attempts, wall), nil
}

// warmConnections establishes n idle keep-alive connections through the run's
// own client, so the race measures the write path rather than TCP setup.
func (r *Runner) warmConnections(ctx context.Context, n int) error {
	const wave = 25
	for established := 0; established < n; established += wave {
		size := min(wave, n-established)

		var (
			wg   sync.WaitGroup
			errs = make([]error, size)
		)
		for i := 0; i < size; i++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+"/healthz", nil)
				if err != nil {
					errs[slot] = err
					return
				}
				resp, err := r.Client.Do(req)
				if err != nil {
					errs[slot] = err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return fmt.Errorf("load: warming %d connections: %w", n, err)
			}
		}
	}
	return nil
}

func (r *Runner) fire(ctx context.Context, body []byte, token string) Attempt {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.BaseURL+"/api/v1/bookings", newReader(body))
	if err != nil {
		return Attempt{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// A fresh key per request. Reusing one would turn the second attempt into an
	// idempotent replay — a 200, not a race.
	req.Header.Set("Idempotency-Key", uuid.NewString())

	started := time.Now()
	resp, err := r.Client.Do(req)
	latency := time.Since(started)
	if err != nil {
		return Attempt{Latency: latency, Err: err}
	}
	// Drain and close, or the connection is not reused and the run measures
	// connection setup instead of contention.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return Attempt{Status: resp.StatusCode, Latency: latency}
}

func summarise(attempts []Attempt, wall time.Duration) *Report {
	rep := &Report{N: len(attempts), Wall: wall, ByStatus: map[int]int{}}

	var confirmed, conflict, shed []time.Duration
	for _, a := range attempts {
		if a.Err != nil {
			rep.Errors++
			continue
		}
		rep.ByStatus[a.Status]++
		switch a.Status {
		case http.StatusCreated, http.StatusOK:
			confirmed = append(confirmed, a.Latency)
		case http.StatusConflict:
			conflict = append(conflict, a.Latency)
		case http.StatusTooManyRequests:
			shed = append(shed, a.Latency)
		}
	}

	rep.ConfirmP99 = p99(confirmed)
	rep.ConflictP99 = p99(conflict)
	rep.ShedP99 = p99(shed)
	return rep
}

// p99 is the nearest-rank 99th percentile. Zero for an empty sample, which the
// threshold check treats as "not measured" rather than "passed".
func p99(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	rank := int(math.Ceil(0.99*float64(len(xs)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(xs) {
		rank = len(xs) - 1
	}
	return xs[rank]
}

// Check applies the thresholds and returns the failures, most important first.
//
// exclusive says whether the target facility is decided by the exclusion
// constraint, in which case exactly one request may be confirmed. For a shared
// facility the winner count is the capacity and this check does not apply.
func (r *Report) Check(exclusive bool) []string {
	return r.check(exclusive, true)
}

// CheckWithoutLatency validates the correctness and transport invariants while
// omitting latency budgets. The load test uses this under -race because race
// instrumentation changes wall-clock measurements; the printed report must
// still reflect the checks that actually ran.
func (r *Report) CheckWithoutLatency(exclusive bool) []string {
	return r.check(exclusive, false)
}

func (r *Report) check(exclusive, checkLatency bool) []string {
	var failures []string

	if n := r.ServerErrors(); n > 0 {
		failures = append(failures, fmt.Sprintf("%d 5xx responses (want 0)", n))
	}
	if r.Errors > 0 {
		failures = append(failures, fmt.Sprintf("%d transport errors (want 0)", r.Errors))
	}
	if exclusive && r.Confirmed() != 1 {
		failures = append(failures,
			fmt.Sprintf("%d confirmations for one exclusive slot (want exactly 1)", r.Confirmed()))
	}

	if !checkLatency {
		r.Failures = failures
		return failures
	}

	// An unmeasured percentile is a failure, not a pass. A run in which nobody
	// lost has not demonstrated that losing is fast.
	switch {
	case r.Conflicts() == 0:
		failures = append(failures, "no 409 responses: the run did not create contention")
	case r.ConflictP99 >= ConflictP99Budget:
		failures = append(failures, fmt.Sprintf("409 p99 %s exceeds %s",
			round(r.ConflictP99), ConflictP99Budget))
	}

	switch {
	case r.Confirmed() == 0:
		failures = append(failures, "no 201 responses: nobody won the slot")
	case r.ConfirmP99 >= ConfirmedP99Budget:
		failures = append(failures, fmt.Sprintf("201 p99 %s exceeds %s",
			round(r.ConfirmP99), ConfirmedP99Budget))
	}

	r.Failures = failures
	return failures
}

func round(d time.Duration) time.Duration { return d.Round(100 * time.Microsecond) }

// Print writes the human-readable split.
func (r *Report) Print(w io.Writer, exclusive bool) {
	fmt.Fprintf(w, "\n  %d requests in %s\n\n", r.N, r.Wall.Round(time.Millisecond))

	statuses := make([]int, 0, len(r.ByStatus))
	for s := range r.ByStatus {
		statuses = append(statuses, s)
	}
	sort.Ints(statuses)
	for _, s := range statuses {
		fmt.Fprintf(w, "  %3d  %5d  %s\n", s, r.ByStatus[s], label(s))
	}
	if r.Errors > 0 {
		fmt.Fprintf(w, "  ---  %5d  transport errors\n", r.Errors)
	}

	fmt.Fprintf(w, "\n  p99 409 (reject)   %8s   budget %s\n", round(r.ConflictP99), ConflictP99Budget)
	fmt.Fprintf(w, "  p99 201 (confirm)  %8s   budget %s\n", round(r.ConfirmP99), ConfirmedP99Budget)
	if r.Shed() > 0 {
		fmt.Fprintf(w, "  p99 429 (shed)     %8s   no budget; shedding is free by construction\n", round(r.ShedP99))
	}
	fmt.Fprintf(w, "  5xx                %8d\n", r.ServerErrors())

	if len(r.Failures) == 0 {
		fmt.Fprintf(w, "\n  PASS\n")
		return
	}
	fmt.Fprintf(w, "\n  FAIL\n")
	for _, f := range r.Failures {
		fmt.Fprintf(w, "    - %s\n", f)
	}
	_ = exclusive
}

func label(status int) string {
	switch status {
	case http.StatusCreated:
		return "confirmed"
	case http.StatusOK:
		return "idempotent replay"
	case http.StatusConflict:
		return "conflict — the exclusion constraint said no"
	case http.StatusTooManyRequests:
		return "shed or rate limited"
	case http.StatusUnprocessableEntity:
		return "validation / fair-use cap"
	default:
		return ""
	}
}

// ctxOrBackground keeps the runner usable from both main and a test.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
