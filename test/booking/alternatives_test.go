package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/config"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Making losing useful — §5.3, §10.3, FR-07, G-3.
//
// At 6 PM most students lose. A bare 409 tells them no and hands them nothing;
// a 409 carrying two or three bookable alternatives converts the loss into a
// second attempt that usually succeeds, because the herd is fighting over one
// slot rather than over the evening.
//
// Everything below also pins the constraint that makes this safe to ship: the
// enrichment is on a hard budget and degrades to a bare 409. It is allowed to be
// absent. It is not allowed to be late.
// ---------------------------------------------------------------------------

const campusTZ = "Asia/Kolkata"

// altService wires the write path with the 409 enrichment attached. grid may be
// nil, which forces the SQL fallback rather than the cached-grid fast path.
func altService(t *testing.T, pg *testutil.PG, grid booking.GridCache) *booking.Service {
	t.Helper()
	return pg.BookingService(t).
		WithAlternatives(booking.NewAlternatives(pg.DB.Replica, grid, campusTZ))
}

// conflictOf asserts err is a lost race and returns the enriched conflict.
func conflictOf(t *testing.T, err error) *booking.Conflict {
	t.Helper()
	require.Error(t, err)
	var c *booking.Conflict
	require.ErrorAs(t, err, &c, "a 409 must carry the Conflict envelope; got %v", err)
	return c
}

// lose books the slot as one student, then attempts it as another and returns
// the loser's conflict. This is the whole scenario in one line.
func lose(t *testing.T, svc *booking.Service, facilityID uuid.UUID, start time.Time, d time.Duration) *booking.Conflict {
	t.Helper()
	ctx := context.Background()

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: facilityID, UserID: testutil.StudentID(0),
		Start: start, Duration: d, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err, "the winner must win")

	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: facilityID, UserID: testutil.StudentID(1),
		Start: start, Duration: d, IdemKey: uuid.NewString(),
	})
	return conflictOf(t, err)
}

// closeSlot writes a manager closure: a BLOCKED row with no user. It is the same
// mechanism a real closure uses (§10.4), so the exclusion constraint keeps
// bookings out of the window and the read path shows it as closed.
func closeSlot(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, start, end time.Time) {
	t.Helper()
	_, err := pg.Pool.Exec(context.Background(), `
		INSERT INTO bookings (facility_id, user_id, is_exclusive, during, status)
		VALUES ($1, NULL, true, tstzrange($2, $3, '[)'), 'BLOCKED')`,
		facilityID, start, end)
	require.NoError(t, err)
}

// fillCounter marks a shared slot full without writing thirty bookings.
func fillCounter(t *testing.T, pg *testutil.PG, facilityID uuid.UUID, start, end time.Time, capacity int) {
	t.Helper()
	_, err := pg.Pool.Exec(context.Background(), `
		INSERT INTO slot_capacity (facility_id, slot_start, slot_end, capacity, booked)
		VALUES ($1, $2, $3, $4, $4)`,
		facilityID, start, end, capacity)
	require.NoError(t, err)
}

// istHours renders the alternatives as local start hours, which is how the
// failure messages read best.
func istHours(alts []booking.Alternative) []int {
	out := make([]int, 0, len(alts))
	for _, a := range alts {
		out = append(out, a.Start.In(testutil.IST).Hour())
	}
	return out
}

func namesOf(alts []booking.Alternative) []string {
	out := make([]string, 0, len(alts))
	for _, a := range alts {
		out = append(out, a.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// The two questions
// ---------------------------------------------------------------------------

// TestAlternatives_SameFacilityLaterSlot is question 1: the court they wanted,
// at the next hour it is actually free. Nearest first — the next free hour is
// worth more to a student than one at closing time.
func TestAlternatives_SameFacilityLaterSlot(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := altService(t, pg, nil) // nil grid => the SQL fallback path

	start, _ := testutil.Slot18()
	c := lose(t, svc, testutil.CourtID(), start, time.Hour)

	require.NotEmpty(t, c.Alternatives, "a 409 with nothing to do next is a dead end")

	first := c.Alternatives[0]
	require.Equal(t, booking.AlternativeLaterHere, first.Kind)
	require.Equal(t, testutil.CourtID(), first.FacilityID)
	require.Equal(t, 19, first.Start.In(testutil.IST).Hour(),
		"nearest free hour first; got %v", istHours(c.Alternatives))
	require.Equal(t, time.Hour, first.End.Sub(first.Start),
		"an alternative keeps the duration the student asked for")

	// Nothing earlier than the slot they lost: 06:00 today is free and useless.
	for _, a := range c.Alternatives {
		require.False(t, a.Start.Before(start),
			"suggested %s, which is not later than the lost slot", a.Start)
	}
}

// TestAlternatives_SameTimeOtherFacilitySameSport is question 2, and it is often
// the better answer: someone who wanted 18:00 tennis usually wanted 18:00 more
// than they wanted that particular court.
func TestAlternatives_SameTimeOtherFacilitySameSport(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := altService(t, pg, nil)

	start, _ := testutil.Slot18()
	c := lose(t, svc, testutil.CourtID(), start, time.Hour)

	var elsewhere []booking.Alternative
	for _, a := range c.Alternatives {
		if a.Kind == booking.AlternativeElsewhere {
			elsewhere = append(elsewhere, a)
		}
	}

	require.Len(t, elsewhere, 1,
		"the same-court answers must not crowd out the different-court one; got %v",
		namesOf(c.Alternatives))
	require.Equal(t, testutil.Court2ID(), elsewhere[0].FacilityID)
	require.True(t, elsewhere[0].Start.Equal(start),
		"a different court is offered at the SAME time; got %s", elsewhere[0].Start)

	// Same sport only. A free badminton court is not an answer to "I wanted
	// tennis", and offering one is worse than offering nothing.
	for _, a := range c.Alternatives {
		require.Equal(t, "tennis", a.Sport, "%s is not tennis", a.Name)
	}
}

// TestAlternatives_MaxThree pins the cap. Three is a decision about the screen:
// a rejected student scanning a list is not shopping, and a fourth option costs
// more attention than it returns.
func TestAlternatives_MaxThree(t *testing.T) {
	pg := testutil.Postgres(t)
	svc := altService(t, pg, nil)

	// Court 1 is free 06:00-22:00, so question 1 alone could supply fifteen.
	start, _ := testutil.Slot18()
	c := lose(t, svc, testutil.CourtID(), start, time.Hour)

	require.LessOrEqual(t, len(c.Alternatives), booking.MaxAlternatives)
	require.Len(t, c.Alternatives, booking.MaxAlternatives,
		"with a whole free evening and a free Court 2 there is no excuse for a short list")

	// And the mix is enforced, not incidental: the same court may not take every
	// place, or "try a different court" would never be offered.
	var here int
	for _, a := range c.Alternatives {
		if a.Kind == booking.AlternativeLaterHere {
			here++
		}
	}
	require.LessOrEqual(t, here, 2, "the same court must not fill the list: %v", namesOf(c.Alternatives))
}

// ---------------------------------------------------------------------------
// What must never be suggested
// ---------------------------------------------------------------------------

// TestAlternatives_ExcludesClosedAndInactive covers the two ways a slot can look
// free in a naive query and be unbookable in reality.
//
// Suggesting something that returns 422 on tap is worse than suggesting nothing:
// it spends the user's second attempt — the one this whole feature exists to
// create — on a door that was never open.
func TestAlternatives_ExcludesClosedAndInactive(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	// Court 2 is withdrawn from service, so question 2 has no answer.
	_, err := pg.Pool.Exec(ctx, `UPDATE facilities SET is_active = false WHERE id = $1`,
		testutil.Court2ID())
	require.NoError(t, err)

	// And 19:00 on Court 1 is closed for maintenance, so question 1 must skip it.
	nineteen, twenty := testutil.Slot(19, time.Hour)
	closeSlot(t, pg, testutil.CourtID(), nineteen, twenty)

	svc := altService(t, pg, nil)
	start, _ := testutil.Slot18()
	c := lose(t, svc, testutil.CourtID(), start, time.Hour)

	for _, a := range c.Alternatives {
		require.NotEqual(t, testutil.Court2ID(), a.FacilityID,
			"an inactive facility must never be suggested")
		require.NotEqual(t, 19, a.Start.In(testutil.IST).Hour(),
			"19:00 is closed; suggestions were %v", istHours(c.Alternatives))
	}

	// The closure is skipped over, not fatal: 20:00 is still an answer.
	require.NotEmpty(t, c.Alternatives)
	require.Equal(t, 20, c.Alternatives[0].Start.In(testutil.IST).Hour(),
		"the next free hour after the closure; got %v", istHours(c.Alternatives))
}

// TestAlternatives_ExcludesFullSharedSlots proves the lookup asks BOTH
// mechanisms' questions.
//
// A full gym slot has no booking row that overlaps it — occupancy there lives in
// the slot_capacity counter, not in the exclusion constraint — so a query that
// only looked at bookings would cheerfully offer a session with nowhere to
// stand.
func TestAlternatives_ExcludesFullSharedSlots(t *testing.T) {
	pg := testutil.Postgres(t)
	gym := testutil.GymID()

	eighteen, nineteen := testutil.Slot(18, time.Hour)
	twenty := nineteen.Add(time.Hour)

	fillCounter(t, pg, gym, eighteen, nineteen, 30) // the slot they want: full
	fillCounter(t, pg, gym, nineteen, twenty, 30)   // and so is the next one

	svc := altService(t, pg, nil)

	_, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: gym, UserID: testutil.StudentID(0),
		Start: eighteen, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.ErrorIs(t, err, booking.ErrCapacityFull, "Mechanism B decides this one")
	c := conflictOf(t, err)

	require.NotEmpty(t, c.Alternatives, "a full gym slot still has a later one")
	for _, a := range c.Alternatives {
		require.NotEqual(t, 19, a.Start.In(testutil.IST).Hour(),
			"19:00 is at capacity; suggestions were %v", istHours(c.Alternatives))
	}
	require.Equal(t, 20, c.Alternatives[0].Start.In(testutil.IST).Hour(),
		"the first slot with room left; got %v", istHours(c.Alternatives))
}

// TestAlternatives_EmptyWhenCampusFull: the 409 is the response. Alternatives
// are decoration, and an empty list is a perfectly good answer when there is
// genuinely nowhere to go.
func TestAlternatives_EmptyWhenCampusFull(t *testing.T) {
	pg := testutil.Postgres(t)

	// Close both tennis courts for the whole day.
	dayStart, _ := testutil.Slot(6, time.Hour)
	dayEnd, _ := testutil.Slot(22, time.Hour)
	closeSlot(t, pg, testutil.CourtID(), dayStart, dayEnd)
	closeSlot(t, pg, testutil.Court2ID(), dayStart, dayEnd)

	svc := altService(t, pg, nil)
	start, _ := testutil.Slot18()

	_, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})

	require.ErrorIs(t, err, booking.ErrSlotTaken, "the closure blocks the booking")
	c := conflictOf(t, err)
	require.Empty(t, c.Alternatives, "nowhere to go is not an error; got %v", namesOf(c.Alternatives))

	// The rest of the envelope still ships. Losing with nothing to offer is
	// still a well-formed 409, and the waitlist is exactly what it is for.
	require.True(t, c.WaitlistAvailable)
	require.Contains(t, c.Error(), "Tennis Court 1")
}

// ---------------------------------------------------------------------------
// The budget
// ---------------------------------------------------------------------------

// slowGrid is a cache lookup that takes 200 ms, which is what a sick Redis or a
// saturated replica looks like from in here. It honours cancellation, as any
// real network call does — the budget is only a budget if the callee stops.
type slowGrid struct{ delay time.Duration }

func (g slowGrid) CampusCached(ctx context.Context, _ string) (*facility.CampusGrid, bool) {
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
	}
	return nil, false
}

// TestAlternatives_TimeoutDegradesGracefully is the load-bearing test in this
// file.
//
// M-3 puts rejections at p99 < 150 ms, tighter than confirmations on purpose,
// because at 6 PM the loser path IS the product. Enriching an error must never
// be the reason it is late. So a slow lookup produces a bare 409 — not a 500,
// not a slow 409, and not an error the user ever sees.
func TestAlternatives_TimeoutDegradesGracefully(t *testing.T) {
	pg := testutil.Postgres(t)

	svc := pg.BookingService(t).WithAlternatives(
		booking.NewAlternatives(pg.DB.Replica, slowGrid{delay: 200 * time.Millisecond}, campusTZ))

	start, _ := testutil.Slot18()
	ctx := context.Background()

	_, err := svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	began := time.Now()
	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(1),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	elapsed := time.Since(began)

	// Still a clean conflict. The lookup failing is not the request failing.
	require.ErrorIs(t, err, booking.ErrSlotTaken)
	c := conflictOf(t, err)
	require.Empty(t, c.Alternatives, "a lookup that missed its budget contributes nothing")
	require.True(t, c.WaitlistAvailable, "the parts that cost nothing still ship")

	// And it did not wait for the slow lookup. 200 ms of delay, 40 ms of budget.
	require.Less(t, elapsed, 150*time.Millisecond,
		"the whole rejection took %s — the budget did not bound the enrichment", elapsed)
	t.Logf("rejection with a 200ms-stalled lookup completed in %s", elapsed)
}

// TestAlternatives_ServedFromWarmCache proves the fast path is the DEFAULT, not
// a nicety.
//
// During a 6 PM burst the campus grid is warm by definition — every student who
// loaded the discovery screen refreshed it seconds ago — so alternatives should
// cost an in-memory scan of a payload this process already holds. Counting the
// queries is the only way to assert that: the answer looks identical either way,
// and two replica round trips per loser, once per member of the herd, is exactly
// the sort of cost that only shows up under load.
func TestAlternatives_ServedFromWarmCache(t *testing.T) {
	pg := testutil.Postgres(t)
	rdb := testutil.Redis(t)
	ctx := context.Background()

	svcOnly := pg.BookingService(t)
	start, _ := testutil.Slot18()

	// The winner books first, so the cached grid already shows 18:00 as taken.
	_, err := svcOnly.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(0),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	require.NoError(t, err)

	// Warm the grid through the real read path, on a pool we are NOT counting.
	// A long TTL so this measures the fast path, not the clock.
	av := facility.NewAvailability(pg.Pool, rdb, campusTZ, nil).WithTTL(30 * time.Second)
	date := start.In(testutil.IST).Format("2006-01-02")
	_, err = av.Campus(ctx, date)
	require.NoError(t, err)

	_, ok := av.CampusCached(ctx, date)
	require.True(t, ok, "the grid must be warm for this test to mean anything")

	// The alternatives lookup gets an instrumented replica. If it issues a single
	// query on a cache hit, the fast path is not the default.
	replica, counter := pg.CountingPool(t)
	require.NoError(t, replica.Ping(ctx))
	counter.Reset()

	svc := pg.BookingService(t).WithAlternatives(booking.NewAlternatives(replica, av, campusTZ))

	_, err = svc.Create(ctx, booking.CreateRequest{
		FacilityID: testutil.CourtID(), UserID: testutil.StudentID(1),
		Start: start, Duration: time.Hour, IdemKey: uuid.NewString(),
	})
	c := conflictOf(t, err)

	require.NotEmpty(t, c.Alternatives, "the in-memory scan must actually find something")
	require.Equal(t, int64(0), counter.Count(),
		"a warm grid must cost ZERO queries; %d were issued", counter.Count())

	// The scan produces the same answer the SQL path does.
	require.Equal(t, 19, c.Alternatives[0].Start.In(testutil.IST).Hour(),
		"got %v", istHours(c.Alternatives))
	require.Contains(t, namesOf(c.Alternatives), "Tennis Court 2")
}

// ---------------------------------------------------------------------------
// The wire contract
// ---------------------------------------------------------------------------

// altAPI is the real router with the 409 enrichment wired, over real Postgres.
//
// Built here rather than reused from test/api because the enrichment is opt-in
// on the service: a Service without it returns a correct bare 409, which is what
// the rest of the suite exercises.
type altAPI struct {
	server *httptest.Server
	pg     *testutil.PG
}

func newAltAPI(t *testing.T) *altAPI {
	t.Helper()

	pg := testutil.Postgres(t)
	cfg := &config.Config{
		DBURL:               pg.DSN,
		DBMaxConns:          20,
		AuthMode:            config.AuthModeDev,
		JWTSecret:           "test-secret",
		WriteQueueDepth:     config.DefaultWriteQueueDepth,
		WriteTimeout:        5 * time.Second,
		TZDisplay:           campusTZ,
		RateLimitIPPerMin:   100000,
		RateLimitUserPerMin: 100000,
	}

	loc, err := time.LoadLocation(cfg.TZDisplay)
	require.NoError(t, err)

	facilities := facility.NewRepo(pg.Pool)
	availability := facility.NewAvailability(pg.DB.Replica, nil, cfg.TZDisplay, nil)
	svc := booking.NewService(pg.DB, facilities, loc).
		WithAlternatives(booking.NewAlternatives(pg.DB.Replica, availability, cfg.TZDisplay))

	srv := httptest.NewServer(httpx.NewRouter(httpx.RouterDeps{
		Config:       cfg,
		DB:           pg.DB,
		Bookings:     svc,
		Facilities:   facilities,
		Availability: availability,
	}))
	t.Cleanup(srv.Close)

	return &altAPI{server: srv, pg: pg}
}

func (a *altAPI) post(t *testing.T, path, token string, body any) (int, []byte) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, a.server.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpx.HeaderIdempotencyKey, uuid.NewString())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := a.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

func (a *altAPI) token(t *testing.T, roll string) string {
	t.Helper()
	status, raw := a.post(t, "/api/v1/dev/login", "", map[string]any{"roll_no": roll})
	require.Equal(t, http.StatusOK, status, "dev login: %s", raw)

	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.Token
}

var hhmm = regexp.MustCompile(`^\d{2}:\d{2}$`)

// TestConflict409_MatchesContract pins the envelope of §10.3 field by field.
//
// The envelope is a contract with a client that is not in this repository. Its
// shape is the one thing a frontend cannot discover by reading the code, so an
// extra field, a renamed one or a timestamp where a clock time was promised is a
// breaking change that no other test in this suite would notice.
func TestConflict409_MatchesContract(t *testing.T) {
	a := newAltAPI(t)
	start, _ := testutil.Slot18()

	body := map[string]any{
		"facility_id":      testutil.CourtID().String(),
		"start":            start.Format(time.RFC3339),
		"duration_minutes": 60,
	}

	status, _ := a.post(t, "/api/v1/bookings", a.token(t, "student01"), body)
	require.Equal(t, http.StatusCreated, status)

	status, raw := a.post(t, "/api/v1/bookings", a.token(t, "student02"), body)
	require.Equal(t, http.StatusConflict, status, "body: %s", raw)

	// Decoded loosely, so an extra field is a failure rather than being silently
	// dropped by a typed struct.
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(raw, &envelope))

	keys := make([]string, 0, len(envelope))
	for k := range envelope {
		keys = append(keys, k)
	}
	require.ElementsMatch(t,
		[]string{"error", "message", "alternatives", "waitlist_available", "request_id"},
		keys, "the 409 envelope is exactly §10.3; got %s", raw)

	require.Equal(t, httpx.CodeSlotTaken, envelope["error"], "machine-readable code")
	require.NotEmpty(t, envelope["message"])
	require.NotEqual(t, envelope["error"], envelope["message"],
		"the client switches on the code and shows the prose; they are different fields")
	require.Contains(t, envelope["message"], "Tennis Court 1",
		"the message names the reason, not just the fact")
	require.Equal(t, true, envelope["waitlist_available"])
	require.NotEmpty(t, envelope["request_id"])

	alts, ok := envelope["alternatives"].([]any)
	require.True(t, ok, "alternatives must be a list; got %T", envelope["alternatives"])
	require.NotEmpty(t, alts)
	require.LessOrEqual(t, len(alts), booking.MaxAlternatives)

	for i, item := range alts {
		alt, ok := item.(map[string]any)
		require.Truef(t, ok, "alternative %d is not an object", i)

		altKeys := make([]string, 0, len(alt))
		for k := range alt {
			altKeys = append(altKeys, k)
		}
		require.ElementsMatchf(t, []string{"facility_id", "name", "start"}, altKeys,
			"alternative %d does not match §10.3", i)

		_, err := uuid.Parse(alt["facility_id"].(string))
		require.NoErrorf(t, err, "alternative %d: facility_id must be a UUID", i)
		require.NotEmptyf(t, alt["name"], "alternative %d: name", i)
		require.Regexpf(t, hhmm, alt["start"],
			"alternative %d: §10.3 shows a local clock time like \"18:00\", got %v", i, alt["start"])
	}

	// Localised at the edge, in IST — never the server's UTC.
	require.Equal(t, "19:00", alts[0].(map[string]any)["start"])
}

// ---------------------------------------------------------------------------
// The budget, under real contention
// ---------------------------------------------------------------------------

// TestConflictLatency_Under150ms is M-3 measured with the enrichment switched on.
//
// The previous tests prove a stalled lookup degrades. This one proves a HEALTHY
// lookup does not quietly cost the target either — 500 requests on one slot,
// through the same shedder production runs, with alternatives computed from the
// cold SQL path (the pessimistic case; a real 6 PM burst has the grid warm).
//
// The claim being defended is narrow and worth stating exactly: the rejection is
// still on time when it is also useful.
func TestConflictLatency_Under150ms(t *testing.T) {
	const n = 500

	pg := testutil.Postgres(t)
	court := testutil.CourtID()
	start, _ := testutil.Slot18()

	cat := testutil.Catalogue(t, pg)
	testutil.WarmCatalogue(t, cat, court)

	svc := pg.BookingServiceWith(t, cat).
		WithAlternatives(booking.NewAlternatives(pg.DB.Replica, nil, campusTZ))

	pg.Warm(t, 25)
	users := pg.Users(t, n)

	shedder := httpx.NewShedder(config.DefaultWriteQueueDepth, 800*time.Millisecond)

	out := testutil.Race(t, n, func(ctx context.Context, i int) (any, error) {
		var b *booking.Booking
		err := shedder.Do(ctx, func(ctx context.Context) error {
			var err error
			b, err = svc.Create(ctx, booking.CreateRequest{
				FacilityID: court, UserID: users[i], Start: start,
				Duration: time.Hour, IdemKey: uuid.NewString(),
			})
			return err
		})
		return b, err
	})

	var conflicts, sheds, other []testutil.Attempt
	for _, at := range out.Attempts {
		switch {
		case at.Err == nil:
		case errors.Is(at.Err, booking.ErrShed):
			sheds = append(sheds, at)
		case errors.Is(at.Err, booking.ErrSlotTaken):
			conflicts = append(conflicts, at)
		default:
			other = append(other, at)
		}
	}

	// The invariant first: enrichment must not have touched who won.
	var confirmed int
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bookings WHERE facility_id = $1 AND status = 'CONFIRMED'`,
		court).Scan(&confirmed))
	require.Equal(t, 1, confirmed, "exactly one winner, alternatives or not")
	require.Len(t, out.Successes(), 1)
	if len(other) > 0 {
		require.Failf(t, "unclassified errors on the loser path",
			"%d of %d attempts failed with something that is neither a conflict nor a shed; first: %v",
			len(other), n, other[0].Err)
	}

	p99 := testutil.Percentile(conflicts, 99)
	t.Logf("n=%d admitted=%d shed=%d  409 p50=%s p95=%s p99=%s",
		n, len(conflicts)+1, len(sheds),
		testutil.Percentile(conflicts, 50),
		testutil.Percentile(conflicts, 95), p99)

	require.NotEmpty(t, conflicts)
	if raceDetectorEnabled {
		// Measured, reported, not enforced. See raceflag_on_test.go: the
		// detector's instrumentation dominates the number, so asserting on it
		// would gate the build on the cost of the tooling rather than on M-3.
		t.Logf("race detector enabled — M-3 reported, not enforced (p99 %s)", p99)
	} else {
		require.Less(t, p99, 150*time.Millisecond,
			"M-3: an enriched rejection must still land inside 150 ms; p99 was %s", p99)
	}

	// And they really were enriched — a fast 409 that carries nothing would pass
	// the latency assertion while quietly not doing the job.
	var enriched int
	for _, at := range conflicts {
		var c *booking.Conflict
		if errors.As(at.Err, &c) && len(c.Alternatives) > 0 {
			enriched++
		}
	}
	require.NotZero(t, enriched,
		"every conflict came back bare — the latency target was met by not doing the work")
	t.Logf("%d of %d conflicts carried alternatives", enriched, len(conflicts))
}
