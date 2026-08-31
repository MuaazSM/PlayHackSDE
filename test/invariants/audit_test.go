// Package invariants_test is the continuous-invariant audit.
//
// Every other suite in this repo asserts that a specific operation behaves. This
// one asserts that the DATABASE IS SANE, whatever happened to it. It takes no
// fixtures for granted, sets up no scenario of its own when pointed at a live
// database, and reads only. That is what makes it runnable mid-demo: point it at
// the machine a judge just hammered with 500 concurrent requests and it will say
// whether the invariants in CLAUDE.md actually held, rather than whether a
// hand-built scenario reproduced.
//
// Two modes, chosen by the environment:
//
//   - AUDIT_DB_URL or DB_URL set — audit THAT database, in whatever state it is
//     in. Nothing is written, nothing is truncated. This is `make audit` and the
//     mode that matters on stage.
//
//   - neither set — start a throwaway Postgres, and first CREATE contended state
//     (a real race, a real capacity burst, a real closure, a real expiring hold)
//     so the audit has something to be wrong about. A clean seeded database
//     satisfies every invariant here trivially, which would make this file a
//     very expensive way to assert nothing.
//
// The invariants are numbered to match CLAUDE.md's non-negotiables.
package invariants_test

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/waitlist"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestMain skips the package under -short, for the same reason test/chaos does:
// in its default mode this suite runs a 200-way race and a 60-way capacity burst
// to have something worth auditing, and `make test` is not the place to spend
// that. `make audit` and `make audit-live` run it.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// db is the audited pool, resolved once per package run.
type db struct {
	pool *pgxpool.Pool
	// live is true when we are auditing somebody else's database and must not
	// write to it.
	live bool
}

var audited *db

// open resolves the database under audit.
//
// The live pool is opened directly rather than through store.New: the audit is
// read-only and deliberately shares no configuration with the write path, so a
// misconfigured pool cannot make an invariant look satisfied.
func open(t *testing.T) *db {
	t.Helper()

	if audited != nil {
		return audited
	}

	url := os.Getenv("AUDIT_DB_URL")
	if url == "" {
		url = os.Getenv("DB_URL")
	}

	if url != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Fatalf("audit: connect %q: %v", url, err)
		}
		if err := pool.Ping(ctx); err != nil {
			t.Fatalf("audit: ping %q: %v", url, err)
		}
		t.Logf("auditing LIVE database (read-only): %s", redact(url))
		audited = &db{pool: pool, live: true}
		audited.describe(t)
		return audited
	}

	t.Log("no AUDIT_DB_URL/DB_URL set — starting a throwaway Postgres and " +
		"generating contended state before auditing")
	pg := testutil.Postgres(t)
	audited = &db{pool: pg.Pool}
	seedContention(t, pg)
	audited.describe(t)
	return audited
}

// describe prints what is actually in the database under audit.
//
// Without this the suite can pass loudly against an empty table and nobody would
// know. A green audit is only worth something next to the row counts it was
// green about, which is also what a judge asks for on stage.
func (d *db) describe(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	var confirmed, held, blocked, cancelled, counters, waiting int
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM bookings WHERE status = 'CONFIRMED'),
		       (SELECT count(*) FROM bookings WHERE status = 'HELD'),
		       (SELECT count(*) FROM bookings WHERE status = 'BLOCKED'),
		       (SELECT count(*) FROM bookings WHERE status = 'CANCELLED'),
		       (SELECT count(*) FROM slot_capacity),
		       (SELECT count(*) FROM waitlist WHERE status = 'WAITING')`).
		Scan(&confirmed, &held, &blocked, &cancelled, &counters, &waiting)
	require.NoError(t, err, "audit: describe")

	t.Logf("state under audit: bookings confirmed=%d held=%d blocked=%d cancelled=%d; "+
		"slot_capacity rows=%d; waitlist waiting=%d",
		confirmed, held, blocked, cancelled, counters, waiting)
}

// redact hides the password in a DSN before it reaches the log.
func redact(url string) string {
	at := -1
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return url
	}
	slashes := -1
	for i := 0; i+2 < len(url) && i < at; i++ {
		if url[i] == '/' && url[i+1] == '/' {
			slashes = i + 2
			break
		}
	}
	if slashes < 0 || slashes >= at {
		return url
	}
	return url[:slashes] + "***" + url[at:]
}

// seedContention puts the throwaway database into a state worth auditing.
//
// It uses the real service, not hand-written INSERTs. An invariant proved
// against rows this file wrote itself would only be testing this file.
func seedContention(t *testing.T, pg *testutil.PG) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	court := testutil.CourtID()
	gym := testutil.GymID()
	start, end := testutil.Slot18()
	_ = end

	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, court, gym)
	svc := pg.BookingServiceWith(t, cat)
	pg.Warm(t, 25)

	// A real 200-way race on one exclusive court. One winner, 199 losers, and
	// a GiST index that has been fought over.
	users := pg.Users(t, 200)
	testutil.Race(t, 200, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: court,
			UserID:     users[i],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
	})

	// A real capacity burst on the gym: 60 requests against capacity 30, so
	// slot_capacity has actually been decremented under contention.
	gymUsers := pg.Users(t, 60)
	testutil.Race(t, 60, func(ctx context.Context, i int) (any, error) {
		return svc.Create(ctx, booking.CreateRequest{
			FacilityID: gym,
			UserID:     gymUsers[i],
			Start:      start,
			Duration:   time.Hour,
			IdemKey:    uuid.NewString(),
		})
	})

	// An idempotent replay, so the (user_id, idem_key) index has a live pair in
	// it rather than only distinct keys.
	replayKey := uuid.NewString()
	replayStart, _ := testutil.Slot(20, time.Hour)
	for i := 0; i < 3; i++ {
		_, _ = svc.Create(ctx, booking.CreateRequest{
			FacilityID: testutil.Court2ID(),
			UserID:     users[0],
			Start:      replayStart,
			Duration:   time.Hour,
			IdemKey:    replayKey,
		})
	}

	// A closure on a third facility, to give the CONFIRMED-vs-BLOCKED invariant
	// a BLOCKED window to be checked against.
	closureStart, closureEnd := testutil.Slot(7, time.Hour)
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
		VALUES ($1, NULL, true, tstzrange($2::timestamptz, $3::timestamptz, '[)'), 'BLOCKED')`,
		testutil.FacilityIDBySlug("badminton-court-1"), closureStart, closureEnd)
	require.NoError(t, err, "audit: seed closure")

	// A HELD row that is still within its window. The orphan invariant must
	// pass on this one; it is here so the query is exercised against a real
	// HELD row rather than an empty table.
	heldStart, heldEnd := testutil.Slot(9, time.Hour)
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status, held_until)
		VALUES ($1, $2, true, tstzrange($3::timestamptz, $4::timestamptz, '[)'), 'HELD', now() + interval '10 minutes')`,
		testutil.FacilityIDBySlug("badminton-court-2"), users[1], heldStart, heldEnd)
	require.NoError(t, err, "audit: seed hold")
}

// ---------------------------------------------------------------------------
// INV-2 — no two overlapping CONFIRMED bookings on an exclusive facility.
// ---------------------------------------------------------------------------

// TestINV2_NoOverlappingExclusiveBookings is non-negotiable #1 and #2 observed
// from the outside: whatever the write path did, the rows it left behind do not
// double-book a court.
//
// The exclusion constraint makes this true by construction, which is exactly why
// it is worth asserting separately — the day someone adds a code path that
// inserts with is_exclusive = false, or relaxes the constraint predicate, the
// constraint stops covering the row and stays green while doing it. This query
// does not consult the constraint. It self-joins the table.
func TestINV2_NoOverlappingExclusiveBookings(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	rows, err := d.pool.Query(ctx, `
		SELECT a.id, b.id, a.facility_id, a.during::text, b.during::text
		  FROM bookings a
		  JOIN bookings b
		    ON a.facility_id = b.facility_id
		   AND a.id < b.id
		   AND a.during && b.during
		 WHERE a.status = 'CONFIRMED'
		   AND b.status = 'CONFIRMED'
		   AND a.is_exclusive
		   AND b.is_exclusive`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var aID, bID, fID uuid.UUID
		var aDuring, bDuring string
		require.NoError(t, rows.Scan(&aID, &bID, &fID, &aDuring, &bDuring))
		offenders = append(offenders,
			"facility "+fID.String()+": "+aID.String()+" "+aDuring+" overlaps "+bID.String()+" "+bDuring)
	}
	require.NoError(t, rows.Err())

	require.Emptyf(t, offenders,
		"INV-2 VIOLATED — the one thing this system exists to prevent has happened:\n  %v",
		offenders)
}

// ---------------------------------------------------------------------------
// INV-3 — no shared slot is oversubscribed.
// ---------------------------------------------------------------------------

// TestINV3_NoOversubscribedSharedSlot checks Mechanism B from the outside.
//
// A CHECK constraint on slot_capacity already forbids booked > capacity, so this
// asserts the same thing twice on purpose: the CHECK protects the counter row,
// and the second half of this test protects the counter's MEANING by comparing
// it to the bookings it claims to count. A counter that is internally legal and
// disagrees with the booking rows is the shared-facility equivalent of a double
// booking, and no constraint catches it.
func TestINV3_NoOversubscribedSharedSlot(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	var over int
	require.NoError(t, d.pool.QueryRow(ctx, `
		SELECT count(*) FROM slot_capacity WHERE booked > capacity`).Scan(&over))
	require.Zerof(t, over, "INV-3 VIOLATED — %d shared slots have booked > capacity", over)

	var negative int
	require.NoError(t, d.pool.QueryRow(ctx, `
		SELECT count(*) FROM slot_capacity WHERE booked < 0`).Scan(&negative))
	require.Zerof(t, negative, "INV-3 VIOLATED — %d shared slots have a negative counter", negative)

	// The counter must equal the CONFIRMED+HELD bookings that overlap its slot.
	// Anything else means a cancel decremented a counter it should not have, or
	// a booking committed without its decrement.
	rows, err := d.pool.Query(ctx, `
		SELECT sc.facility_id, sc.slot_start, sc.booked, sc.capacity,
		       (SELECT count(*) FROM bookings b
		         WHERE b.facility_id = sc.facility_id
		           AND NOT b.is_exclusive
		           AND b.status IN ('CONFIRMED','HELD')
		           AND b.during && tstzrange(sc.slot_start, sc.slot_end, '[)')) AS actual
		  FROM slot_capacity sc`)
	require.NoError(t, err)
	defer rows.Close()

	type drift struct {
		facility uuid.UUID
		start    time.Time
		booked   int
		actual   int
		capacity int
	}
	var drifts []drift
	for rows.Next() {
		var dr drift
		require.NoError(t, rows.Scan(&dr.facility, &dr.start, &dr.booked, &dr.capacity, &dr.actual))
		if dr.booked != dr.actual {
			drifts = append(drifts, dr)
		}
	}
	require.NoError(t, rows.Err())

	for _, dr := range drifts {
		t.Errorf("INV-3 VIOLATED — counter drift: facility %s slot %s says booked=%d (cap %d) "+
			"but %d booking rows overlap it",
			dr.facility, dr.start.In(testutil.IST).Format(time.RFC3339), dr.booked, dr.capacity, dr.actual)
	}
}

// ---------------------------------------------------------------------------
// INV-4 — derived availability agrees with the bookings table, for every slot.
// ---------------------------------------------------------------------------

// TestINV4_AvailabilityMatchesBookings is non-negotiable #4 under continuous
// audit: there is no is_available column, so availability can only be a function
// of the bookings table, and this asserts the function is the one we think.
//
// It runs the REAL read path — facility.Availability over the same pool the API
// uses — and compares every slot of every facility, for every date that has any
// booking on it, against an oracle written in a deliberately different shape:
// one targeted count per slot, no generated grid, no LATERAL. Two queries that
// share a bug are one query; these two share nothing but the table.
func TestINV4_AvailabilityMatchesBookings(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	repo := facility.NewRepo(d.pool)
	// nil Redis on purpose. The cache is not authoritative (non-negotiable #3)
	// and an audit that consulted it would be auditing the cache.
	av := facility.NewAvailability(d.pool, nil, "Asia/Kolkata", nil)

	dates := auditDates(t, d)
	require.NotEmpty(t, dates, "audit: no dates to check — is the database empty?")
	t.Logf("checking %d date(s): %v", len(dates), dates)

	facilities, err := repo.List(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, facilities, "audit: no facilities — has the seed run?")

	checked := 0
	for _, date := range dates {
		for i := range facilities {
			f := &facilities[i]

			day, err := av.ForFacility(ctx, f, date)
			require.NoErrorf(t, err, "availability for %s on %s", f.Name, date)

			for _, s := range day.Slots {
				want := oracleState(t, d, f, s.Start, s.End)
				require.Equalf(t, want, s.State,
					"INV-4 VIOLATED — derived availability disagrees with the bookings "+
						"table: %s on %s at %s reads %q, bookings say %q",
					f.Name, date, s.Start.In(testutil.IST).Format("15:04"), s.State, want)
				checked++
			}
		}
	}
	t.Logf("INV-4: %d slots agree with the bookings table", checked)
}

// auditDates is every local date that has a booking on it, plus today — so an
// empty database still exercises the query rather than vacuously passing.
func auditDates(t *testing.T, d *db) []string {
	t.Helper()
	ctx := context.Background()

	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT to_char(lower(during) AT TIME ZONE 'Asia/Kolkata', 'YYYY-MM-DD')
		  FROM bookings
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var date string
		require.NoError(t, rows.Scan(&date))
		if !seen[date] {
			seen[date] = true
			out = append(out, date)
		}
	}
	require.NoError(t, rows.Err())

	today := time.Now().In(testutil.IST).Format("2006-01-02")
	if !seen[today] {
		out = append(out, today)
	}
	return out
}

// oracleState recomputes one slot's state straight from the bookings table.
//
// This is intentionally the dumbest possible implementation of the rule. If it
// ever grows a join or a CTE it has started to resemble the thing it is meant to
// independently check, and it stops being an oracle.
func oracleState(t *testing.T, d *db, f *facility.Facility, start, end time.Time) string {
	t.Helper()
	ctx := context.Background()

	var blocked, held, confirmed int
	require.NoError(t, d.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'BLOCKED'),
		       count(*) FILTER (WHERE status = 'HELD'),
		       count(*) FILTER (WHERE status = 'CONFIRMED')
		  FROM bookings
		 WHERE facility_id = $1
		   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		f.ID, start, end).Scan(&blocked, &held, &confirmed))

	if blocked > 0 {
		return facility.StateClosed
	}

	if f.IsExclusive {
		switch {
		case confirmed > 0:
			return facility.StateBooked
		case held > 0:
			return facility.StateHeld
		default:
			return facility.StateFree
		}
	}

	// Shared: occupancy is the counter row, not the booking count. A missing
	// counter row means the slot has never been booked, which is empty.
	var booked, capacity int
	if err := d.pool.QueryRow(ctx, `
		SELECT booked, capacity FROM slot_capacity
		 WHERE facility_id = $1 AND slot_start = $2`, f.ID, start).Scan(&booked, &capacity); err != nil {
		booked, capacity = 0, f.Capacity
	}

	switch {
	case capacity > 0 && booked >= capacity:
		return facility.StateFull
	case booked > 0:
		return facility.StateFilling
	default:
		return facility.StateFree
	}
}

// ---------------------------------------------------------------------------
// INV-5 — idempotency keys are unique per user.
// ---------------------------------------------------------------------------

// TestINV5_NoDuplicateIdemKey is non-negotiable #5 observed from the outside: a
// retry returns the original result, which is only possible if there is exactly
// one row to return.
//
// A partial unique index makes this true, and the index is the thing most likely
// to be quietly dropped by a future migration that "cleans up unused indexes".
// This test does not use it.
func TestINV5_NoDuplicateIdemKey(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	rows, err := d.pool.Query(ctx, `
		SELECT user_id, idem_key, count(*)
		  FROM bookings
		 WHERE idem_key IS NOT NULL AND user_id IS NOT NULL
		 GROUP BY user_id, idem_key
		HAVING count(*) > 1`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var user uuid.UUID
		var key string
		var n int
		require.NoError(t, rows.Scan(&user, &key, &n))
		offenders = append(offenders,
			user.String()+" / "+key+" appears "+itoa(n)+" times")
	}
	require.NoError(t, rows.Err())

	require.Emptyf(t, offenders,
		"INV-5 VIOLATED — a replayed Idempotency-Key created a second booking:\n  %v",
		offenders)
}

// ---------------------------------------------------------------------------
// A CONFIRMED booking may never sit inside a BLOCKED window.
// ---------------------------------------------------------------------------

// TestNoConfirmedInsideClosure is the closure invariant. A manager closing a
// facility for maintenance must not leave a student holding a booking inside the
// closed window: either the closure was refused, or the affected bookings were
// cancelled with it.
//
// Note this deliberately does NOT filter on is_exclusive. On an exclusive
// facility the exclusion constraint already makes the overlap impossible; on a
// SHARED facility it does not — the constraint predicate excludes shared rows
// entirely, so nothing in the schema stops a gym booking surviving a gym
// closure. That case is the whole reason this test exists.
func TestNoConfirmedInsideClosure(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	rows, err := d.pool.Query(ctx, `
		SELECT c.id, b.id, b.facility_id, b.is_exclusive, b.during::text, c.during::text
		  FROM bookings c
		  JOIN bookings b
		    ON b.facility_id = c.facility_id
		   AND b.during && c.during
		   AND b.id <> c.id
		 WHERE c.status = 'BLOCKED'
		   AND b.status = 'CONFIRMED'`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var closureID, bookingID, facilityID uuid.UUID
		var exclusive bool
		var bDuring, cDuring string
		require.NoError(t, rows.Scan(&closureID, &bookingID, &facilityID, &exclusive, &bDuring, &cDuring))
		kind := "shared"
		if exclusive {
			kind = "exclusive"
		}
		offenders = append(offenders,
			"booking "+bookingID.String()+" "+bDuring+" survives "+kind+" closure "+
				closureID.String()+" "+cDuring+" on facility "+facilityID.String())
	}
	require.NoError(t, rows.Err())

	require.Emptyf(t, offenders,
		"VIOLATED — CONFIRMED bookings overlap a BLOCKED window:\n  %v", offenders)
}

// ---------------------------------------------------------------------------
// No orphaned HELD row.
// ---------------------------------------------------------------------------

// TestNoOrphanedHolds checks that the sweepers are actually running.
//
// A HELD row past its held_until is a court nobody can book and nobody is using.
// The waitlist sweeper reclaims these every waitlist.SweepInterval, so a row a
// few seconds past its deadline is normal and not a bug. The tolerance is
// therefore ONE sweep interval plus a small margin: anything older than that
// means the sweeper is not running, not that it is late.
//
// The margin is not slop. A sweep that starts just before a hold expires will
// not see it, so the true worst case is two intervals; the margin below is
// deliberately smaller than that so a genuinely stopped sweeper is still caught
// on the next audit rather than never.
func TestNoOrphanedHolds(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	const margin = 15 * time.Second
	tolerance := waitlist.SweepInterval + margin

	rows, err := d.pool.Query(ctx, `
		SELECT id, facility_id, held_until, now() - held_until
		  FROM bookings
		 WHERE status = 'HELD'
		   AND held_until < now() - $1::interval`,
		tolerance.String())
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var id, facilityID uuid.UUID
		var heldUntil time.Time
		var overdue time.Duration
		require.NoError(t, rows.Scan(&id, &facilityID, &heldUntil, &overdue))
		offenders = append(offenders,
			"hold "+id.String()+" on facility "+facilityID.String()+
				" expired "+overdue.String()+" ago and was never reclaimed")
	}
	require.NoError(t, rows.Err())

	require.Emptyf(t, offenders,
		"VIOLATED — HELD rows more than one sweep interval (%s) past held_until. "+
			"The waitlist sweeper is not running:\n  %v", tolerance, offenders)
}

// ---------------------------------------------------------------------------
// Structural invariants that no amount of traffic should be able to break.
// ---------------------------------------------------------------------------

// TestNoIsAvailableColumn is non-negotiable #4 asserted against the live
// catalogue rather than against the migration files. A grep proves nobody wrote
// the column into a migration; this proves nobody added it by hand at 2am.
func TestNoIsAvailableColumn(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	rows, err := d.pool.Query(ctx, `
		SELECT table_name, column_name
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND column_name ILIKE '%is_available%'`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		offenders = append(offenders, table+"."+column)
	}
	require.NoError(t, rows.Err())

	require.Emptyf(t, offenders,
		"non-negotiable #4 VIOLATED — an availability column exists: %v", offenders)
}

// TestExclusionConstraintStillExists guards the mechanism itself.
//
// Every other invariant here would still pass for a while after somebody dropped
// no_double_book — right up until two people booked the same court. This asserts
// the constraint is present, is an EXCLUDE, and is still scoped by the predicate
// that keeps it off shared facilities.
func TestExclusionConstraintStillExists(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	var kind, def string
	err := d.pool.QueryRow(ctx, `
		SELECT c.contype::text, pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relname = 'bookings' AND c.conname = 'no_double_book'`).Scan(&kind, &def)
	require.NoError(t, err, "no_double_book is GONE — Mechanism A no longer exists")

	require.Equal(t, "x", kind, "no_double_book is no longer an EXCLUDE constraint: %s", def)
	require.Contains(t, def, "gist", "no_double_book no longer uses GiST: %s", def)
	require.Contains(t, def, "&&", "no_double_book no longer tests range overlap: %s", def)
	require.Contains(t, def, "is_exclusive",
		"no_double_book is no longer scoped to exclusive facilities — it would now "+
			"reject the second gym booking before Mechanism B runs: %s", def)
	t.Logf("no_double_book: %s", def)
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
