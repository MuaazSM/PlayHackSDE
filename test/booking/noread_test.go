package booking_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/booking"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestCreate_NeverReadsOccupancy enforces non-negotiable #2: no SELECT of slot
// occupancy before the INSERT.
//
// This needs its own test because a read-then-write is invisible to every
// behavioural test in this suite. The write path serialises per facility, so a
// check-then-insert would still yield exactly one winner and pass the 500-way
// race — and would then fail in production the moment the serialisation was
// relaxed or the check moved outside the lock. The gap between the SELECT and
// the INSERT is the bug the whole design exists to eliminate, and the only way
// to prove it is absent is to look at the statements actually issued.
func TestCreate_NeverReadsOccupancy(t *testing.T) {
	pg := testutil.Postgres(t)

	db, rec := pg.RecordingDB(t)
	cat := facility.NewRepo(db.Primary)
	svc := booking.NewService(db, cat, testutil.IST)

	court := testutil.CourtID()
	testutil.WarmCatalogue(t, cat, court)

	start, _ := testutil.Slot18()

	rec.Reset()
	b, err := svc.Create(context.Background(), booking.CreateRequest{
		FacilityID: court,
		UserID:     testutil.StudentID(0),
		Start:      start,
		Duration:   time.Hour,
		IdemKey:    uuid.NewString(),
	})
	require.NoError(t, err)
	require.NotNil(t, b)

	stmts := rec.Statements()
	require.NotEmpty(t, stmts, "the recorder must have seen the write path")

	for i, sql := range stmts {
		normalised := strings.ToUpper(strings.Join(strings.Fields(stripLineComments(sql)), " "))

		if !strings.HasPrefix(normalised, "SELECT") {
			continue
		}
		// The only SELECT the happy path may run is the advisory lock, which
		// reads no table at all.
		if strings.Contains(normalised, "PG_ADVISORY_XACT_LOCK") {
			continue
		}
		require.NotContainsf(t, normalised, "FROM BOOKINGS",
			"statement %d reads the bookings table on the write path — that is the "+
				"read-then-write gap non-negotiable #2 forbids:\n%s", i, sql)
	}

	// And positively: the write really is a bare INSERT.
	var sawInsert bool
	for _, sql := range stmts {
		if strings.Contains(strings.ToUpper(sql), "INSERT INTO BOOKINGS") {
			sawInsert = true
		}
	}
	require.True(t, sawInsert, "the write path must insert into bookings")

	t.Logf("write path issued %d statements", len(stmts))
	for i, sql := range stmts {
		t.Logf("  %d: %s", i, firstLine(stripLineComments(sql)))
	}
}

func stripLineComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if len(t) > 90 {
				return t[:90] + "..."
			}
			return t
		}
	}
	return ""
}
