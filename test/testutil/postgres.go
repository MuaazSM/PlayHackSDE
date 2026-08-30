// Package testutil is the shared test harness: real Postgres, real Redis, real
// fixtures, and a race helper with correct release-together semantics.
//
// Nothing here mocks the database. A mock cannot raise a 23P01, which means a
// mocked test of this system proves nothing about the only thing being tested.
package testutil

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/seed"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PG is a live Postgres with the schema migrated and the demo data seeded.
type PG struct {
	DB   *store.DB
	Pool *pgxpool.Pool
	DSN  string
}

var (
	pgOnce sync.Once
	pgInst *PG
	pgErr  error
)

// Postgres returns a Postgres with migrations applied and the seed loaded.
//
// One container is started per package run and reused; the data is truncated and
// re-seeded on every call, so each test sees the same clean starting state.
// Because state is shared, tests using this must not call t.Parallel().
//
// The container is reaped by Ryuk when the test binary exits.
func Postgres(t *testing.T) *PG {
	t.Helper()

	pgOnce.Do(func() { pgInst, pgErr = startPostgres() })
	if pgErr != nil {
		t.Fatalf("testutil: postgres: %v", pgErr)
	}

	pgInst.Reset(t)
	return pgInst
}

func startPostgres() (*PG, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("playhack_test"),
		tcpostgres.WithUsername("playhack"),
		tcpostgres.WithPassword("playhack"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("starting container: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}

	if err := migrateUp(dsn); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}

	// Exercise the real pool constructor, so the tests run against the same
	// pgx configuration production uses.
	db, err := store.New(ctx, &config.Config{
		DBURL:      dsn,
		DBMaxConns: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}

	return &PG{DB: db, Pool: db.Primary, DSN: dsn}, nil
}

// Reset truncates every table and re-applies the seed.
func (p *PG) Reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.truncateAll(ctx); err != nil {
		t.Fatalf("testutil: truncate: %v", err)
	}
	if _, err := seed.Run(ctx, p.Pool); err != nil {
		t.Fatalf("testutil: seed: %v", err)
	}
}

// truncateAll discovers the table list from the catalogue rather than hardcoding
// it, so a new migration cannot silently leave a table un-truncated between
// tests.
func (p *PG) truncateAll(ctx context.Context) error {
	rows, err := p.Pool.Query(ctx, `
		SELECT quote_ident(tablename)
		  FROM pg_tables
		 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(tables) == 0 {
		return fmt.Errorf("no tables found — did migrations run?")
	}

	_, err = p.Pool.Exec(ctx,
		"TRUNCATE TABLE "+strings.Join(tables, ", ")+" RESTART IDENTITY CASCADE")
	return err
}

// Warm opens n connections up front so a race does not measure connection setup
// instead of contention.
func (p *PG) Warm(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := p.Pool.Acquire(ctx)
		if err != nil {
			break
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

func migrateUp(dsn string) error {
	dir, err := MigrationsDir()
	if err != nil {
		return err
	}

	// pgx5:// selects golang-migrate's pgx/v5 driver. Migrations always connect
	// direct to Postgres — golang-migrate takes a session advisory lock, which a
	// transaction-mode pooler cannot honour.
	m, err := migrate.New("file://"+dir, strings.Replace(dsn, "postgres://", "pgx5://", 1))
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return err
	}
	return nil
}

// MigrationsDir resolves /migrations from this source file, so tests do not
// depend on the working directory.
func MigrationsDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve caller")
	}
	// test/testutil/postgres.go -> repo root
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return filepath.Join(root, "migrations"), nil
}
