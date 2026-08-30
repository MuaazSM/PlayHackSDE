// Package store owns the pgx pool, transaction helpers and — in pgerr.go — the
// single place in the codebase that inspects a SQLSTATE string.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions configures a pgx pool. Values come from config, never from the
// environment directly.
type PoolOptions struct {
	URL         string
	MaxConns    int32
	MinConns    int32
	MaxConnLife time.Duration
	MaxConnIdle time.Duration
}

// NewPool opens and verifies a pgx pool.
//
// The pool normally points at PgBouncer in transaction mode, so nothing here may
// depend on session state. Prepared-statement caching is disabled for that
// reason: named prepared statements do not survive a backend being handed to a
// different client between transactions.
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
// error or panic.
//
// Handlers must never open a transaction directly, and a transaction must never
// contain a network call or a notification send — keep them short.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
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
