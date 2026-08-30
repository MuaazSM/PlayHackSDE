package schema_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/seed"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// SQLSTATE codes we assert on by name rather than by string literal at the
// call site. Asserting the code — not merely "an error occurred" — is the whole
// point: a NOT NULL violation and an exclusion violation are both errors, and
// only one of them means the constraint is doing its job.
const (
	sqlstateExclusionViolation  = "23P01"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)

// insertBooking is the raw write. Deliberately a plain INSERT with no preceding
// SELECT — the read-then-write gap is the bug this schema exists to remove.
const insertBooking = `
INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
VALUES ($1, $2, $3, tstzrange($4, $5, '[)'), $6::booking_status)
RETURNING id`

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newFacility inserts a fresh facility so each test is isolated from the others.
func newFacility(t *testing.T, ctx context.Context, sport string, exclusive bool, capacity int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := shared.pool.QueryRow(ctx, `
		INSERT INTO facilities (name, sport, is_exclusive, capacity, opens_at, closes_at)
		VALUES ($1, $2, $3, $4, '05:00', '23:00')
		RETURNING id`,
		sport+"-"+uuid.NewString()[:8], sport, exclusive, capacity,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func newUser(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	suffix := uuid.NewString()[:8]
	var id uuid.UUID
	err := shared.pool.QueryRow(ctx, `
		INSERT INTO users (roll_no, name, email)
		VALUES ($1, $2, $3)
		RETURNING id`,
		"roll-"+suffix, "Test "+suffix, "test-"+suffix+"@iitg.ac.in",
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// slot builds a UTC window on a fixed future date, so tests never depend on
// today's clock.
func slot(hour, durationHours int) (time.Time, time.Time) {
	start := time.Date(2030, 1, 15, hour, 0, 0, 0, time.UTC)
	return start, start.Add(time.Duration(durationHours) * time.Hour)
}

// requireSQLSTATE asserts that err is a Postgres error carrying exactly code.
func requireSQLSTATE(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	require.Error(t, err, "expected a database error with SQLSTATE %s, got nil", code)

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected *pgconn.PgError, got %T: %v", err, err)
	require.Equal(t, code, pgErr.Code,
		"expected SQLSTATE %s, got %s (%s: %s)", code, pgErr.Code, pgErr.ConstraintName, pgErr.Message)
	return pgErr
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestMigrations_UpDownUp proves the down migrations are real, not decorative.
// A down path that leaves debris makes every later "clean database" claim a lie.
func TestMigrations_UpDownUp(t *testing.T) {
	ctx := ctxT(t)

	// Own container: this test tears the schema down, which would break the
	// tests sharing the migrated one.
	p, err := startPostgres(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.terminate(context.Background()) })

	for cycle := 1; cycle <= 2; cycle++ {
		require.NoErrorf(t, migrateUp(p.dsn), "cycle %d: up", cycle)
		assertSchemaPresent(t, p, cycle)

		require.NoErrorf(t, migrateDownAll(p.dsn), "cycle %d: down", cycle)
		assertSchemaClean(t, p, cycle)
	}

	// Third up, so the sequence really is up -> down -> up.
	require.NoError(t, migrateUp(p.dsn), "final up")
	assertSchemaPresent(t, p, 3)
}

func assertSchemaPresent(t *testing.T, p *pg, cycle int) {
	t.Helper()
	ctx := ctxT(t)

	want := []string{
		"users", "facilities", "bookings", "slot_capacity",
		"waitlist", "check_ins", "policies", "outbox", "booking_events",
	}
	for _, table := range want {
		var exists bool
		require.NoError(t, p.pool.QueryRow(ctx,
			`SELECT to_regclass('public.'||$1) IS NOT NULL`, table).Scan(&exists))
		require.Truef(t, exists, "cycle %d: table %s missing after up", cycle, table)
	}

	var constraints int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname = 'no_double_book'`).Scan(&constraints))
	require.Equalf(t, 1, constraints, "cycle %d: no_double_book missing after up", cycle)
}

// assertSchemaClean is the actual contract: after down, nothing of ours is left
// — no tables, no enums, no functions, no extensions.
func assertSchemaClean(t *testing.T, p *pg, cycle int) {
	t.Helper()
	ctx := ctxT(t)

	// schema_migrations is golang-migrate's own bookkeeping and legitimately
	// survives a full down.
	var tables []string
	rows, err := p.pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		ORDER BY tablename`)
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())
	require.Emptyf(t, tables, "cycle %d: tables left behind after down: %v", cycle, tables)

	var enums []string
	rows, err = p.pool.Query(ctx, `
		SELECT t.typname FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public' AND t.typtype = 'e'
		ORDER BY 1`)
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		enums = append(enums, n)
	}
	require.NoError(t, rows.Err())
	require.Emptyf(t, enums, "cycle %d: enum types left behind after down: %v", cycle, enums)

	var fnCount int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_proc p
		 JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'public' AND p.proname = 'notify_outbox'`).Scan(&fnCount))
	require.Equalf(t, 0, fnCount, "cycle %d: notify_outbox() left behind after down", cycle)

	var extCount int
	require.NoError(t, p.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname IN ('btree_gist','pgcrypto','citext')`).Scan(&extCount))
	require.Equalf(t, 0, extCount, "cycle %d: extensions left behind after down", cycle)
}

// TestBtreeGistAvailable is the hour-one gate. If this fails in a deploy
// environment, stop and escalate — the fallback is a materially weaker product.
func TestBtreeGistAvailable(t *testing.T) {
	ctx := ctxT(t)

	for _, ext := range []string{"btree_gist", "pgcrypto", "citext"} {
		var version string
		err := shared.pool.QueryRow(ctx,
			`SELECT extversion FROM pg_extension WHERE extname = $1`, ext).Scan(&version)
		require.NoErrorf(t, err, "extension %s not installed after migration 0001", ext)
		require.NotEmpty(t, version)
		t.Logf("%s %s", ext, version)
	}

	// The constraint index must actually be a GiST index — a btree would accept
	// the DDL for the equality column and reject the range operator.
	var method string
	require.NoError(t, shared.pool.QueryRow(ctx, `
		SELECT am.amname
		FROM pg_constraint c
		JOIN pg_class i ON i.oid = c.conindid
		JOIN pg_am am ON am.oid = i.relam
		WHERE c.conname = 'no_double_book'`).Scan(&method))
	require.Equal(t, "gist", method)
}

// TestExclusionConstraint_RejectsOverlap is Mechanism A. Two overlapping
// CONFIRMED bookings on one court: the second must be rejected by the database,
// with SQLSTATE 23P01 specifically.
func TestExclusionConstraint_RejectsOverlap(t *testing.T) {
	ctx := ctxT(t)

	court := newFacility(t, ctx, "tennis", true, 1)
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)

	start, end := slot(18, 1)

	var first uuid.UUID
	require.NoError(t, shared.pool.QueryRow(ctx, insertBooking,
		court, alice, true, start, end, "CONFIRMED").Scan(&first))

	// Exactly the same window.
	_, err := shared.pool.Exec(ctx, insertBooking, court, bob, true, start, end, "CONFIRMED")
	pgErr := requireSQLSTATE(t, err, sqlstateExclusionViolation)
	require.Equal(t, "no_double_book", pgErr.ConstraintName)

	// Partial overlap: 18:30-19:30 against 18:00-19:00.
	_, err = shared.pool.Exec(ctx, insertBooking,
		court, bob, true, start.Add(30*time.Minute), end.Add(30*time.Minute), "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateExclusionViolation)

	// Fully containing: 17:00-21:00.
	_, err = shared.pool.Exec(ctx, insertBooking,
		court, bob, true, start.Add(-time.Hour), end.Add(2*time.Hour), "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateExclusionViolation)

	var n int
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`, court).Scan(&n))
	require.Equal(t, 1, n, "exactly one booking must survive")
}

// TestAdjacentSlotsDoNotCollide guards the '[)' bounds decision.
//
// This looks trivial and it is the single most valuable test here: with '[]'
// bounds, 18:00-19:00 and 19:00-20:00 would share their endpoint and the second
// booking would be rejected. That failure looks exactly like the bug the
// exclusion constraint exists to prevent, which makes it maximally confusing to
// debug on stage.
func TestAdjacentSlotsDoNotCollide(t *testing.T) {
	ctx := ctxT(t)

	court := newFacility(t, ctx, "badminton", true, 1)
	alice := newUser(t, ctx)
	bob := newUser(t, ctx)

	eighteen, nineteen := slot(18, 1)
	_, twenty := slot(19, 1)

	_, err := shared.pool.Exec(ctx, insertBooking, court, alice, true, eighteen, nineteen, "CONFIRMED")
	require.NoError(t, err, "18:00-19:00 must insert")

	_, err = shared.pool.Exec(ctx, insertBooking, court, bob, true, nineteen, twenty, "CONFIRMED")
	require.NoError(t, err, "19:00-20:00 must insert cleanly next to 18:00-19:00 — check the '[)' bounds")

	// And back-to-back in the other direction: 17:00-18:00.
	seventeen, _ := slot(17, 1)
	_, err = shared.pool.Exec(ctx, insertBooking, court, alice, true, seventeen, eighteen, "CONFIRMED")
	require.NoError(t, err, "17:00-18:00 must insert cleanly before 18:00-19:00")

	var n int
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE facility_id = $1`, court).Scan(&n))
	require.Equal(t, 3, n, "three adjacent hours must all be bookable")
}

// TestSharedFacilityNotBlockedByExclusion proves the §3.2 scoping fix.
//
// With the PRD's unscoped predicate, the second gym booking for the same hour
// would be rejected by the exclusion constraint and Mechanism B would be
// unreachable dead code. Both rows must insert.
func TestSharedFacilityNotBlockedByExclusion(t *testing.T) {
	ctx := ctxT(t)

	gym := newFacility(t, ctx, "gym", false, 30)
	start, end := slot(18, 1)

	const members = 5
	for i := 0; i < members; i++ {
		u := newUser(t, ctx)
		_, err := shared.pool.Exec(ctx, insertBooking, gym, u, false, start, end, "CONFIRMED")
		require.NoErrorf(t, err,
			"gym booking %d for the same hour must insert; the exclusion constraint must not "+
				"apply to shared facilities (IMPLEMENTATION.md §3.2)", i+1)
	}

	var n int
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`, gym).Scan(&n))
	require.Equal(t, members, n)

	// The exclusive court next door still rejects its second booking — the fix
	// scopes the constraint, it does not disable it.
	court := newFacility(t, ctx, "tennis", true, 1)
	_, err := shared.pool.Exec(ctx, insertBooking, court, newUser(t, ctx), true, start, end, "CONFIRMED")
	require.NoError(t, err)
	_, err = shared.pool.Exec(ctx, insertBooking, court, newUser(t, ctx), true, start, end, "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateExclusionViolation)
}

// TestIsExclusiveCannotDrift proves the composite FK keeps the denormalised flag
// honest. Without it, a booking row could claim is_exclusive=false on an
// exclusive court and quietly opt itself out of the constraint.
func TestIsExclusiveCannotDrift(t *testing.T) {
	ctx := ctxT(t)

	court := newFacility(t, ctx, "football", true, 1) // is_exclusive = true
	gym := newFacility(t, ctx, "gym", false, 30)      // is_exclusive = false
	u := newUser(t, ctx)
	start, end := slot(18, 1)

	// Lying downward on an exclusive facility is the dangerous direction: it
	// would exempt the row from no_double_book.
	_, err := shared.pool.Exec(ctx, insertBooking, court, u, false, start, end, "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateForeignKeyViolation)

	// Lying upward on a shared facility is equally rejected.
	_, err = shared.pool.Exec(ctx, insertBooking, gym, u, true, start, end, "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateForeignKeyViolation)

	// The facility row itself cannot become inconsistent either.
	_, err = shared.pool.Exec(ctx,
		`INSERT INTO facilities (name, sport, is_exclusive, capacity, opens_at, closes_at)
		 VALUES ('bad', 'tennis', true, 30, '06:00', '22:00')`)
	requireSQLSTATE(t, err, sqlstateCheckViolation)
}

// TestClosureRequiresNullUser holds the CHECK that ties BLOCKED rows to a NULL
// user. A closure is a booking with no booker; a real booking always has one.
func TestClosureRequiresNullUser(t *testing.T) {
	ctx := ctxT(t)

	court := newFacility(t, ctx, "cricket", true, 1)
	u := newUser(t, ctx)
	start, end := slot(9, 2)

	// Valid closure: BLOCKED with user_id NULL.
	var closure uuid.UUID
	require.NoError(t, shared.pool.QueryRow(ctx, insertBooking,
		court, nil, true, start, end, "BLOCKED").Scan(&closure))

	// BLOCKED with a user is not a closure.
	_, err := shared.pool.Exec(ctx, insertBooking, court, u, true, start, end, "BLOCKED")
	requireSQLSTATE(t, err, sqlstateCheckViolation)

	// CONFIRMED without a user is not a booking.
	later, laterEnd := slot(14, 1)
	_, err = shared.pool.Exec(ctx, insertBooking, court, nil, true, later, laterEnd, "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateCheckViolation)

	// And the closure blocks bookings inside its window.
	_, err = shared.pool.Exec(ctx, insertBooking, court, u, true, start, end, "CONFIRMED")
	requireSQLSTATE(t, err, sqlstateExclusionViolation)
}

// TestSeedIdempotent runs the seed twice and asserts nothing multiplied. The
// seed is re-run mid-demo; if it were not idempotent that would be the moment it
// showed.
func TestSeedIdempotent(t *testing.T) {
	ctx := ctxT(t)

	first, err := seed.Run(ctx, shared.pool)
	require.NoError(t, err)
	require.Equal(t, 7, first.Facilities)
	require.Equal(t, 12, first.Users)
	require.Equal(t, 1, first.Policies)

	before := seededCounts(t, ctx)

	second, err := seed.Run(ctx, shared.pool)
	require.NoError(t, err, "seed must be safe to re-run")
	require.Equal(t, first, second)

	require.Equal(t, before, seededCounts(t, ctx), "re-running the seed must not change row counts")

	// Six exclusive, one shared gym at capacity 30 — §0.
	var exclusive, shared30 int
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE is_exclusive),
		        count(*) FILTER (WHERE NOT is_exclusive AND capacity = 30)
		 FROM facilities WHERE id = ANY($1)`, seededFacilityIDs()).Scan(&exclusive, &shared30))
	require.Equal(t, 6, exclusive)
	require.Equal(t, 1, shared30)

	// Roles landed correctly.
	var students, managers, secretaries int
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE role = 'STUDENT'),
		        count(*) FILTER (WHERE role = 'MANAGER'),
		        count(*) FILTER (WHERE role = 'SECRETARY')
		 FROM users WHERE id = ANY($1)`, seededUserIDs()).Scan(&students, &managers, &secretaries))
	require.Equal(t, 10, students)
	require.Equal(t, 1, managers)
	require.Equal(t, 1, secretaries)
}

type counts struct{ Facilities, Users, Policies int }

func seededCounts(t *testing.T, ctx context.Context) counts {
	t.Helper()
	var c counts
	require.NoError(t, shared.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM facilities WHERE id = ANY($1)),
		        (SELECT count(*) FROM users      WHERE id = ANY($2)),
		        (SELECT count(*) FROM policies)`,
		seededFacilityIDs(), seededUserIDs()).Scan(&c.Facilities, &c.Users, &c.Policies))
	return c
}

func seededFacilityIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(seed.Facilities))
	for _, f := range seed.Facilities {
		ids = append(ids, f.ID())
	}
	return ids
}

func seededUserIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(seed.Users))
	for _, u := range seed.Users {
		ids = append(ids, u.ID())
	}
	return ids
}
