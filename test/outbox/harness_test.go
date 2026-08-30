// Package outbox_test proves non-negotiable #7 against a real Postgres.
//
// It has to be a real one. The property under test is that pg_notify fires on
// COMMIT and not on statement, and that FOR UPDATE SKIP LOCKED hands two
// dispatchers disjoint batches. A mock has neither, so a mocked test of this
// package would assert only that the Go code calls the Go code.
package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// quiet keeps a dispatcher's own logging out of the test output. The assertions
// read the recorder and the database, never the log.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// delivery is one call into the notifier, with when it happened and what the
// transport said about it.
type delivery struct {
	msg outbox.Message
	at  time.Time
	err error
}

// recorder is a Notifier that remembers every send and can be told to fail.
//
// fail is consulted per message, so a test can make the first attempt fail and
// the second succeed — which is the only way to observe a retry actually being
// retried rather than merely being marked.
type recorder struct {
	mu   sync.Mutex
	got  []delivery
	fail func(outbox.Message) error
}

func newRecorder(fail func(outbox.Message) error) *recorder {
	return &recorder{fail: fail}
}

func (r *recorder) Notify(_ context.Context, msg outbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var err error
	if r.fail != nil {
		err = r.fail(msg)
	}
	r.got = append(r.got, delivery{msg: msg, at: time.Now(), err: err})
	return err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// deliveries returns a copy, so a caller can read it without racing the
// dispatcher goroutine still writing to it.
func (r *recorder) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

// ids returns every outbox id seen, in delivery order and WITH duplicates. The
// duplicates are the point in the SKIP LOCKED test.
func (r *recorder) ids() []int64 {
	out := []int64{}
	for _, d := range r.deliveries() {
		out = append(out, d.msg.ID)
	}
	return out
}

// startDispatcher runs a dispatcher for the duration of the test.
//
// Cleanup cancels it AND waits for Run to return. Waiting matters: these tests
// share one Postgres container and one schema, so a dispatcher still draining
// after its test ended would eat the next test's rows and fail it somewhere
// unrelated.
func startDispatcher(t *testing.T, pg *testutil.PG, opt outbox.Options) *outbox.Dispatcher {
	t.Helper()

	if opt.Logger == nil {
		opt.Logger = quiet()
	}
	d := outbox.NewDispatcher(pg.DB, opt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("dispatcher did not stop within 15s of cancellation")
		}
	})

	return d
}

// awaitReady blocks until the LISTEN subscription is live.
//
// Without this a notify-latency assertion is really a race against connection
// setup: the row would be found by the catch-up drain or the ticker, and the
// test would pass or fail on scheduling rather than on the mechanism.
func awaitReady(t *testing.T, d *outbox.Dispatcher) {
	t.Helper()
	select {
	case <-d.Ready():
	case <-time.After(15 * time.Second):
		t.Fatal("dispatcher never established its LISTEN subscription")
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// outboxRow is the state of one row, read straight from the table rather than
// from anything the dispatcher reports about itself.
type outboxRow struct {
	ID       int64
	Topic    string
	Status   string
	Attempts int
	SentAt   *time.Time
}

func readRow(t *testing.T, pg *testutil.PG, id int64) outboxRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var r outboxRow
	err := pg.Pool.QueryRow(ctx,
		`SELECT id, topic, status, attempts, sent_at FROM outbox WHERE id = $1`, id).
		Scan(&r.ID, &r.Topic, &r.Status, &r.Attempts, &r.SentAt)
	require.NoError(t, err)
	return r
}

func countOutbox(t *testing.T, pg *testutil.PG) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var n int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n))
	return n
}
