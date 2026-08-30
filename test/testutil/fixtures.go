package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/seed"
)

// IST is the campus timezone. Everything is stored as timestamptz in UTC and
// localised only at the edge; these helpers exist so a test can say "18:00" and
// mean the slot a student would actually see.
var IST = time.FixedZone("IST", 5*60*60+30*60)

// Seeded facilities. Reset re-applies the seed before every test, so these ids
// are always present and always the same.
func CourtID() uuid.UUID  { return facilityID("tennis-court-1") } // exclusive
func Court2ID() uuid.UUID { return facilityID("tennis-court-2") } // the 409 alternative
func GymID() uuid.UUID    { return facilityID("gymnasium") }      // shared, capacity 30

func facilityID(slug string) uuid.UUID {
	for _, f := range seed.Facilities {
		if f.Slug == slug {
			return f.ID()
		}
	}
	panic("testutil: unknown seeded facility " + slug)
}

// StudentIDs returns the ten seeded student accounts.
func StudentIDs() []uuid.UUID {
	out := make([]uuid.UUID, 0, 10)
	for _, u := range seed.Users {
		if u.Role == "STUDENT" {
			out = append(out, u.ID())
		}
	}
	return out
}

// StudentID returns the i-th seeded student, wrapping so a race with more
// goroutines than seeded users still gets a valid id.
func StudentID(i int) uuid.UUID {
	ids := StudentIDs()
	return ids[i%len(ids)]
}

// Users creates n additional students and returns their ids. Use this when a
// race needs more distinct bookers than the ten seeded ones — every request in
// the race demo must come from a different user, or the idempotency index
// rather than the exclusion constraint would be doing the rejecting.
func (p *PG) Users(t *testing.T, n int) []uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		suffix := uuid.NewString()[:12]
		var id uuid.UUID
		err := p.Pool.QueryRow(ctx, `
			INSERT INTO users (roll_no, name, email)
			VALUES ($1, $2, $3)
			RETURNING id`,
			"t-"+suffix, "Test "+suffix, "t-"+suffix+"@iitg.ac.in",
		).Scan(&id)
		if err != nil {
			t.Fatalf("testutil: create user %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Facility inserts a one-off facility, for tests that need one outside the seed.
func (p *PG) Facility(t *testing.T, sport string, exclusive bool, capacity int) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var id uuid.UUID
	err := p.Pool.QueryRow(ctx, `
		INSERT INTO facilities (name, sport, is_exclusive, capacity, opens_at, closes_at)
		VALUES ($1, $2, $3, $4, '05:00', '23:00')
		RETURNING id`,
		sport+"-"+uuid.NewString()[:8], sport, exclusive, capacity,
	).Scan(&id)
	if err != nil {
		t.Fatalf("testutil: create facility: %v", err)
	}
	return id
}

// Slot returns [start, end) for today at the given IST hour, in UTC.
//
// Half-open by construction, matching the '[)' bounds the schema relies on:
// 18:00-19:00 and 19:00-20:00 must not overlap.
func Slot(hour int, duration time.Duration) (start, end time.Time) {
	now := time.Now().In(IST)
	start = time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, IST).UTC()
	return start, start.Add(duration)
}

// Slot18 is the contended slot: today at 18:00 IST for one hour. This is the
// one every race test fights over.
func Slot18() (start, end time.Time) { return Slot(18, time.Hour) }

// TSTZRange renders [start, end) as a Postgres tstzrange literal, for the few
// places that bind a range as a single parameter instead of building it in SQL.
func TSTZRange(start, end time.Time) string {
	const layout = "2006-01-02 15:04:05.999999-07"
	return "[" + start.UTC().Format(layout) + "," + end.UTC().Format(layout) + ")"
}
