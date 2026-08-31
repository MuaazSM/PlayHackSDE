// Package schema_test verifies the DDL against a real PostgreSQL 16.
//
// A mock cannot exercise an exclusion constraint, a composite foreign key or a
// CHECK. Testing this layer against anything other than real Postgres would
// prove nothing about the only thing being tested.
package schema_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// shared is a container migrated once and reused by the constraint tests. Each
// test picks its own facility and time window, so they do not interact.
var shared *pg

type pg struct {
	container *tcpostgres.PostgresContainer
	dsn       string
	pool      *pgxpool.Pool
}

func TestMain(m *testing.M) {
	// testing.Short() reads a flag, so the flags must be parsed first.
	flag.Parse()

	if testing.Short() {
		fmt.Fprintln(os.Stderr, "schema: skipping, -short set (needs docker)")
		os.Exit(0)
	}

	ctx := context.Background()

	p, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schema: could not start postgres: %v\n", err)
		os.Exit(1)
	}

	if err := migrateUp(p.dsn); err != nil {
		fmt.Fprintf(os.Stderr, "schema: migrate up: %v\n", err)
		_ = p.terminate(ctx)
		os.Exit(1)
	}

	shared = p
	code := m.Run()

	_ = p.terminate(ctx)
	os.Exit(code)
}

// startPostgres brings up a disposable postgres:16-alpine and returns a pool.
func startPostgres(ctx context.Context) (*pg, error) {
	c, err := tcpostgres.Run(ctx,
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
		return nil, err
	}

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}

	return &pg{container: c, dsn: dsn, pool: pool}, nil
}

func (p *pg) terminate(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.pool != nil {
		p.pool.Close()
	}
	if p.container == nil {
		return nil
	}
	return p.container.Terminate(ctx)
}

func resolveMigrationsDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve caller")
	}
	// test/schema/main_test.go -> repo root
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	dir := filepath.Join(root, "migrations")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("migrations dir %s: %w", dir, err)
	}
	return dir, nil
}

// newMigrator builds a golang-migrate instance bound to the given DSN.
//
// It connects DIRECTLY to postgres, never through PgBouncer: golang-migrate
// takes a session-level advisory lock, which a transaction-mode pooler cannot
// honour.
//
// The iofs source reads migrations through an fs.FS rather than a file URL:
// golang-migrate's file driver keeps the leading slash of a file:///C:/... URL,
// a path Windows cannot resolve, which broke this suite on a fresh clone.
func newMigrator(dsn string) (*migrate.Migrate, error) {
	dir, err := resolveMigrationsDir()
	if err != nil {
		return nil, err
	}
	src, err := iofs.New(os.DirFS(dir), ".")
	if err != nil {
		return nil, err
	}
	return migrate.NewWithSourceInstance("iofs", src, pgx5DSN(dsn))
}

// pgx5DSN rewrites a postgres:// URL onto golang-migrate's pgx/v5 driver.
func pgx5DSN(dsn string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(dsn, prefix) {
			return "pgx5://" + strings.TrimPrefix(dsn, prefix)
		}
	}
	return dsn
}

func migrateUp(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func migrateDownAll(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
