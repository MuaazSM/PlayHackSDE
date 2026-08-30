// Package store owns the pgx pools, the transaction helper, and — in pgerr.go —
// the single place in the codebase that inspects a SQLSTATE string.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB holds the primary and replica pools.
//
// Writes always go to Primary. Availability reads may go to Replica, which is
// the same pool as Primary when DB_REPLICA_URL is unset — the read/write split
// is a config flag, not something the demo depends on existing.
type DB struct {
	Primary *pgxpool.Pool
	Replica *pgxpool.Pool

	dedicatedReplica bool
}

// New opens the pools described by cfg and verifies both are reachable.
func New(ctx context.Context, cfg *config.Config) (*DB, error) {
	primary, err := NewPool(ctx, PoolOptions{
		URL:         cfg.DBURL,
		MaxConns:    cfg.DBMaxConns,
		MaxConnLife: time.Hour,
		MaxConnIdle: 5 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("store: primary: %w", err)
	}

	db := &DB{Primary: primary, Replica: primary}

	// Empty or identical replica URL means "no split" — share the pool rather
	// than opening a second one to the same server.
	if cfg.DBReplicaURL == "" || cfg.DBReplicaURL == cfg.DBURL {
		return db, nil
	}

	replica, err := NewPool(ctx, PoolOptions{
		URL:         cfg.DBReplicaURL,
		MaxConns:    cfg.DBMaxConns,
		MaxConnLife: time.Hour,
		MaxConnIdle: 5 * time.Minute,
	})
	if err != nil {
		primary.Close()
		return nil, fmt.Errorf("store: replica: %w", err)
	}

	db.Replica = replica
	db.dedicatedReplica = true
	return db, nil
}

// HasDedicatedReplica reports whether reads are actually served by a second
// server, or are falling back to the primary.
func (db *DB) HasDedicatedReplica() bool { return db.dedicatedReplica }

// Health pings every distinct pool. It backs /readyz: a replica that cannot
// reach Postgres cannot serve traffic and should be pulled out of rotation.
func (db *DB) Health(ctx context.Context) error {
	if err := db.Primary.Ping(ctx); err != nil {
		return fmt.Errorf("store: primary unreachable: %w", err)
	}
	if db.dedicatedReplica {
		if err := db.Replica.Ping(ctx); err != nil {
			return fmt.Errorf("store: replica unreachable: %w", err)
		}
	}
	return nil
}

// Close releases every distinct pool.
func (db *DB) Close() {
	if db == nil {
		return
	}
	if db.dedicatedReplica && db.Replica != nil {
		db.Replica.Close()
	}
	if db.Primary != nil {
		db.Primary.Close()
	}
}

// PoolOptions configures a single pgx pool. Values come from config, never from
// the environment directly.
type PoolOptions struct {
	URL         string
	MaxConns    int32
	MinConns    int32
	MaxConnLife time.Duration
	MaxConnIdle time.Duration

	// Tracer observes every query on the pool. Left nil in production; tests use
	// it to assert that a code path issued no queries at all.
	Tracer pgx.QueryTracer
}

// NewPool opens and verifies one pgx pool.
//
// The pool normally points at PgBouncer in transaction mode, so nothing here may
// depend on session state. Statement caching is disabled for that reason: a
// backend is handed to a different client between transactions, and named
// prepared statements do not survive that.
func NewPool(ctx context.Context, opt PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(opt.URL)
	if err != nil {
		return nil, fmt.Errorf("store: parse db url: %w", err)
	}

	if opt.MaxConns > 0 {
		cfg.MaxConns = opt.MaxConns
	}
	if opt.MinConns > 0 {
		cfg.MinConns = opt.MinConns
	}
	if opt.MaxConnLife > 0 {
		cfg.MaxConnLifetime = opt.MaxConnLife
	}
	if opt.MaxConnIdle > 0 {
		cfg.MaxConnIdleTime = opt.MaxConnIdle
	}

	if opt.Tracer != nil {
		cfg.ConnConfig.Tracer = opt.Tracer
	}

	// Required when talking through a transaction-mode pooler.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.ConnConfig.StatementCacheCapacity = 0
	cfg.ConnConfig.DescriptionCacheCapacity = 0

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// Querier is the subset of pgx used by repositories. Both *pgxpool.Pool and
// pgx.Tx satisfy it, so the same query code runs inside and outside a
// transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WithTx runs fn inside a transaction, committing on success and rolling back on
// error or panic. This is the only way anything in the codebase opens a
// transaction.
//
// Keep transactions short: no network calls, no notification sends. A panic is
// rolled back and then re-raised — swallowing it would turn a bug into a silent
// no-op.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			// Rollback with a background context: ctx may already be cancelled,
			// and an un-rolled-back transaction would leak the connection.
			rollback(tx)
			panic(p)
		}
		if err != nil {
			rollback(tx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// rollback releases the transaction on a context that cannot already be done.
//
// Once a statement raises inside a transaction the transaction is aborted and no
// further query runs on that connection until rollback — so on 23P01 the caller
// must return here first, then compute alternatives on a fresh connection
// (IMPLEMENTATION.md §4.5).
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
