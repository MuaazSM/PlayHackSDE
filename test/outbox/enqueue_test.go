package outbox_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestEnqueue_RequiresTransaction guards the type-level half of non-negotiable
// #7.
//
// The real assertion is a COMPILE-TIME one and cannot be written as a passing
// test, because the code that would prove it does not build:
//
//	var pool *pgxpool.Pool
//	outbox.Enqueue(ctx, pool, outbox.TopicBookingConfirmed, payload)
//	// cannot use pool (variable of type *pgxpool.Pool) as pgx.Tx value:
//	//   *pgxpool.Pool does not implement pgx.Tx (missing method Commit)
//
// That is the guarantee IMPLEMENTATION.md §8 asks for: an outbox row cannot be
// written outside a transaction, so it cannot commit independently of the
// booking it describes, so a notification cannot describe a booking that never
// happened. It holds by type-checking rather than by anybody remembering it.
//
// What this test can do is stop the guarantee being widened away. Someone
// relaxing the parameter to store.Querier — which *pgxpool.Pool DOES satisfy —
// would silently restore the bug, and nothing else in the suite would notice.
// So: assert the parameter is exactly pgx.Tx, and that a pool does not satisfy
// it.
func TestEnqueue_RequiresTransaction(t *testing.T) {
	fn := reflect.TypeOf(outbox.Enqueue)
	require.Equal(t, reflect.Func, fn.Kind())
	require.Equal(t, 4, fn.NumIn(), "Enqueue(ctx, tx, topic, payload)")

	txType := reflect.TypeOf((*pgx.Tx)(nil)).Elem()
	require.Equal(t, txType, fn.In(1),
		"Enqueue's second parameter must be pgx.Tx exactly. Widening it to an "+
			"interface a *pgxpool.Pool satisfies (store.Querier, for one) would "+
			"let an outbox row commit outside its booking transaction.")

	require.False(t, reflect.TypeOf((*pgxpool.Pool)(nil)).Implements(txType),
		"*pgxpool.Pool now satisfies pgx.Tx, so passing the pool to Enqueue "+
			"compiles and non-negotiable #7 is no longer enforced by the type system")

	// Belt and braces: the interface a pool does satisfy is not what Enqueue
	// takes. This is the exact widening the doc comment warns against.
	querierType := reflect.TypeOf((*store.Querier)(nil)).Elem()
	require.True(t, reflect.TypeOf((*pgxpool.Pool)(nil)).Implements(querierType),
		"assumption behind this test: a pool satisfies store.Querier")
	require.NotEqual(t, querierType, fn.In(1))
}

// promotedPayload is a realistic side-effect body: ids, a window, and the
// deadline that makes a promotion actionable.
type promotedPayload struct {
	EntryID      uuid.UUID `json:"entry_id"`
	BookingID    uuid.UUID `json:"booking_id"`
	FacilityID   uuid.UUID `json:"facility_id"`
	UserID       uuid.UUID `json:"user_id"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	OfferExpires time.Time `json:"offer_expires"`
	Position     int64     `json:"position"`
}

// TestPayloadRoundTrip checks that what a producer enqueues is what a transport
// receives, through jsonb and back.
//
// Not a formality. The insert binds the payload as a string and casts it with
// ::jsonb precisely because the pool runs in QueryExecModeExec, where a []byte
// would bind as bytea and arrive as a hex blob — a bug that looks like nothing
// at all until a notification renders as "\x7b2265..." to a student. Timestamps
// are in the payload for the same reason: they cross the boundary as RFC 3339
// text, and a promotion whose deadline drifted would be worse than one that was
// never sent.
func TestPayloadRoundTrip(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	start, end := testutil.Slot18()
	want := promotedPayload{
		EntryID:      uuid.New(),
		BookingID:    uuid.New(),
		FacilityID:   testutil.CourtID(),
		UserID:       testutil.StudentID(0),
		Start:        start.UTC(),
		End:          end.UTC(),
		OfferExpires: start.UTC().Add(10 * time.Minute),
		Position:     87,
	}

	rec := newRecorder(nil)
	d := startDispatcher(t, pg, outbox.Options{
		Notifier:  rec,
		ListenDSN: pg.DSN,
		Interval:  time.Second,
	})
	awaitReady(t, d)

	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicWaitlistPromoted, want)
	}))

	waitFor(t, 10*time.Second, "the payload to be delivered",
		func() bool { return rec.count() == 1 })

	msg := rec.deliveries()[0].msg
	require.Equal(t, outbox.TopicWaitlistPromoted, msg.Topic)
	require.Equal(t, 1, msg.Attempts)

	// Raw jsonb, not a hex-encoded bytea. json.Valid is the cheap version of the
	// bug described above.
	require.True(t, json.Valid(msg.Payload), "payload is not valid JSON: %q", string(msg.Payload))

	var got promotedPayload
	require.NoError(t, msg.Decode(&got))

	require.Equal(t, want.EntryID, got.EntryID)
	require.Equal(t, want.BookingID, got.BookingID)
	require.Equal(t, want.FacilityID, got.FacilityID)
	require.Equal(t, want.UserID, got.UserID)
	require.Equal(t, want.Position, got.Position)

	// Times compare by instant, not by representation: the wire format is UTC
	// RFC 3339 and the location on the far side is not part of the value.
	require.True(t, want.Start.Equal(got.Start), "start %s != %s", want.Start, got.Start)
	require.True(t, want.End.Equal(got.End), "end %s != %s", want.End, got.End)
	require.True(t, want.OfferExpires.Equal(got.OfferExpires),
		"offer_expires %s != %s", want.OfferExpires, got.OfferExpires)
}
