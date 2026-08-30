package testutil

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QueryCounter counts every query issued on a pool.
//
// It exists for one assertion: that a rejection which should be decided in
// memory does not reach the database. "Cheap rejections never touch the index"
// is a performance claim, and a performance claim with no test is a wish.
type QueryCounter struct{ n atomic.Int64 }

// TraceQueryStart implements pgx.QueryTracer.
func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (c *QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count returns the number of queries seen so far.
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// Reset zeroes the counter. Call it after warm-up so connection setup and cache
// priming are not counted against the path under test.
func (c *QueryCounter) Reset() { c.n.Store(0) }

// CountingPool returns a second pool over the same database, instrumented to
// count queries.
func (p *PG) CountingPool(t *testing.T) (*pgxpool.Pool, *QueryCounter) {
	t.Helper()

	counter := &QueryCounter{}
	pool, err := newTracedPool(p.DSN, counter)
	if err != nil {
		t.Fatalf("testutil: counting pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

// SQLRecorder records every statement issued on a pool, in order.
//
// It backs the assertion that the booking write path never reads occupancy
// before writing. That rule is non-negotiable #2 and is otherwise unenforceable:
// a read-then-write passes every behavioural test as long as something else
// serialises the writers, and only fails in production under real contention.
type SQLRecorder struct {
	mu   sync.Mutex
	sqls []string
}

// TraceQueryStart implements pgx.QueryTracer.
func (r *SQLRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sqls = append(r.sqls, data.SQL)
	r.mu.Unlock()
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (r *SQLRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Statements returns the recorded SQL in order.
func (r *SQLRecorder) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sqls...)
}

// Reset clears the recording.
func (r *SQLRecorder) Reset() {
	r.mu.Lock()
	r.sqls = nil
	r.mu.Unlock()
}

// RecordingPool returns a pool that records every statement it runs.
func (p *PG) RecordingPool(t *testing.T) (*pgxpool.Pool, *SQLRecorder) {
	t.Helper()

	rec := &SQLRecorder{}
	pool, err := newTracedPool(p.DSN, rec)
	if err != nil {
		t.Fatalf("testutil: recording pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, rec
}

// RecordingDB wraps a recording pool as a store.DB, so a service can be built
// over it.
func (p *PG) RecordingDB(t *testing.T) (*store.DB, *SQLRecorder) {
	t.Helper()
	pool, rec := p.RecordingPool(t)
	return &store.DB{Primary: pool, Replica: pool}, rec
}
