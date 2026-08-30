package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/policy"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// entryStatus reads a queue entry's status and the booking it was offered, if
// any.
func entryStatus(t *testing.T, pg *testutil.PG, entryID uuid.UUID) (status string, bookingID *uuid.UUID) {
	t.Helper()
	require.NoError(t, pg.Pool.QueryRow(context.Background(),
		`SELECT status::text, booking_id FROM waitlist WHERE id = $1`, entryID).
		Scan(&status, &bookingID))
	return status, bookingID
}

// TestPriority_InstitutePromotedBeforeIndividual is the tier system's reason to
// exist: §11's ranking has to actually reorder a queue, not merely be stored.
//
// The individual joins FIRST. FIFO alone would promote them. The institute team
// joins second and is promoted anyway, because waitlist_claim_head orders by
// (priority DESC, position ASC) — tier first, arrival second. If the tier were
// not reaching waitlist.priority, this test would promote the wrong student and
// the whole of §11 would be decoration.
func TestPriority_InstitutePromotedBeforeIndividual(t *testing.T) {
	pg := testutil.Postgres(t)
	svc, wl := cappedWithPromotion(t, pg)
	ctx := context.Background()

	court := testutil.CourtID()
	holder := testutil.StudentID(0)
	individual := testutil.StudentID(1)
	institute := testutil.StudentID(2)

	setTier(t, pg, individual, policy.TierIndividual)
	setTier(t, pg, institute, policy.TierInstituteTeam)

	// Somebody is on the court.
	held := mustBook(t, ctx, svc, court, holder, 1, 18)
	start, end := slot(1, 18, time.Hour)

	// The individual queues first.
	first, err := wl.Join(ctx, individual, court, start, end)
	require.NoError(t, err)
	require.Equal(t, 0, first.Priority)

	// The institute team queues second.
	second, err := wl.Join(ctx, institute, court, start, end)
	require.NoError(t, err)
	require.Equal(t, 2, second.Priority)
	require.Greater(t, second.Position, first.Position, "the tier must not be faking an earlier arrival")

	// The court is given back. Promotion runs inside the cancelling transaction.
	_, err = svc.Cancel(ctx, held.ID, holder, "freeing the court")
	require.NoError(t, err)

	instituteStatus, offered := entryStatus(t, pg, second.ID)
	require.Equal(t, "PROMOTED", instituteStatus, "the institute team should have been promoted first")
	require.NotNil(t, offered)

	individualStatus, notOffered := entryStatus(t, pg, first.ID)
	require.Equal(t, "WAITING", individualStatus, "the individual keeps their place, behind the higher tier")
	require.Nil(t, notOffered)

	// The offer is a real HELD booking for the promoted student, not a note in a
	// queue: a promotion that reserved nothing would be an offer the system
	// cannot honour.
	var owner uuid.UUID
	var status string
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT user_id, status::text FROM bookings WHERE id = $1`, *offered).Scan(&owner, &status))
	require.Equal(t, institute, owner)
	require.Equal(t, "HELD", status)
}

// TestPriority_NoShowsSubtract closes §11's feedback loop: "repeat no-shows
// subtract, which is how §7 feeds back into fairness".
//
// A student who books the 6 PM court and does not turn up has taken it from
// somebody who would have used it. The consequence is proportionate — they lose
// ground in the queue for the next contended slot, not the ability to book — and
// it is bounded, because a fairness mechanism nobody can recover from is a
// punishment mechanism.
func TestPriority_NoShowsSubtract(t *testing.T) {
	pg := testutil.Postgres(t)
	_, wl := cappedWithPromotion(t, pg)
	ctx := context.Background()

	court := testutil.CourtID()
	start, end := slot(1, 18, time.Hour)

	clean := testutil.StudentID(1)
	oneNoShow := testutil.StudentID(2)
	repeat := testutil.StudentID(3)
	persistent := testutil.StudentID(4)
	ancient := testutil.StudentID(5)

	for _, u := range []uuid.UUID{clean, oneNoShow, repeat, persistent, ancient} {
		setTier(t, pg, u, policy.TierInstituteTeam)
	}

	addNoShow(t, pg, oneNoShow, court, 2)

	addNoShow(t, pg, repeat, court, 2)
	addNoShow(t, pg, repeat, court, 5)

	// Four recent no-shows, but the penalty is capped.
	for _, daysAgo := range []int{1, 2, 3, 4} {
		addNoShow(t, pg, persistent, court, daysAgo)
	}

	// Older than the lookback: forgiven, and forgiven without anything having to
	// run to forgive it — the count simply stops matching.
	addNoShow(t, pg, ancient, court, int(policy.NoShowLookback/(24*time.Hour))+3)

	for _, tc := range []struct {
		name string
		user uuid.UUID
		want int
	}{
		{"no no-shows keeps the full tier", clean, 2},
		{"one no-show costs a place", oneNoShow, 1},
		{"repeat no-shows cost two", repeat, 0},
		{"the penalty is bounded", persistent, 2 - policy.MaxNoShowPenalty},
		{"an aged-out no-show is forgiven", ancient, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.Priority(ctx, pg.Pool, tc.user)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	// And the number actually lands on the queue row, which is the only place it
	// changes anybody's outcome.
	entry, err := wl.Join(ctx, repeat, court, start, end)
	require.NoError(t, err)
	require.Equal(t, 0, entry.Priority)
}

// TestPriority_TierRankingIsInstituteHostelIndividual pins the ranking §11
// states, at the boundary where it is stored.
//
// Worth its own assertion because the ordering lives in Go while the tier lives
// in Postgres: if the enum ever grew a member or the Base mapping were edited,
// this is what notices.
func TestPriority_TierRankingIsInstituteHostelIndividual(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	student := testutil.StudentID(0)

	for _, tc := range []struct {
		tier policy.Tier
		want int
	}{
		{policy.TierInstituteTeam, 2},
		{policy.TierHostelTeam, 1},
		{policy.TierIndividual, 0},
	} {
		setTier(t, pg, student, tc.tier)
		got, err := policy.Priority(ctx, pg.Pool, student)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "tier %s", tc.tier)
	}

	// Everyone starts as an individual, so migration 0010 is behaviour
	// preserving on the queue that existed before it.
	var seeded int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE tier <> 'INDIVIDUAL' AND id <> $1`, student).Scan(&seeded))
	require.Equal(t, 0, seeded)
}
