// Package outbox is the transactional side-effect queue.
//
// Nothing sends a notification from a request handler. A row is written here
// inside the booking transaction and a worker drains it after commit
// (CLAUDE.md non-negotiable #7).
//
// The AFTER INSERT trigger installed by migration 0006 calls pg_notify, which is
// delivered on COMMIT rather than on statement. That is what makes the pattern
// honest: a notification physically cannot be dispatched for a booking that
// rolled back.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Topics published by the system.
//
// Every side effect the product has is named here, including the ones whose
// emitting feature is not built yet. The dispatcher is topic-agnostic — it moves
// rows — so a topic costs nothing until something enqueues it, and having the
// full set in one place is what stops a later phase inventing a second spelling
// of "booking.no_show".
const (
	// TopicBookingConfirmed — a slot was won, or a promotion offer was claimed.
	TopicBookingConfirmed = "booking.confirmed"

	// TopicBookingCancelled — a confirmed booking was given up or revoked.
	TopicBookingCancelled = "booking.cancelled"

	// TopicWaitlistPromoted — a freed window was offered to the head of a queue.
	// Time-critical: the student has PROMOTION_TTL_MIN to claim it.
	TopicWaitlistPromoted = "waitlist.promoted"

	// TopicWaitlistExpired — a promotion offer was not claimed inside its window
	// and the held slot was released. Emitted by the waitlist sweeper (§6.3).
	//
	// Not in §8's original list, which named only the topics with a NOTIFICATION
	// behind them; nobody is messaged about an offer they let lapse. It is here
	// because §9's live stream needs it: without it a slot that was offered and
	// never claimed stays "held" on every connected grid until the page is
	// reloaded, which is a visibly wrong screen rather than a missing message.
	TopicWaitlistExpired = "waitlist.expired"

	// TopicBookingReminder — the slot starts shortly. Emitted by the reminder
	// sweeper (Phase 11, check-in); no producer yet.
	TopicBookingReminder = "booking.reminder"

	// TopicBookingNoShow — the grace period passed with no check-in and the slot
	// was released. Emitted by the no-show sweeper (Phase 11); no producer yet.
	TopicBookingNoShow = "booking.no_show"

	// TopicClosureCreated — a manager blocked a window and existing bookings
	// inside it were revoked (Phase 12, closures); no producer yet.
	TopicClosureCreated = "closure.created"
)

// Enqueue writes a side effect inside the caller's transaction.
//
// The second parameter is pgx.Tx, and that is the entire point of this
// signature: *pgxpool.Pool does not implement pgx.Tx, so
//
//	outbox.Enqueue(ctx, pool, ...)   // does not compile
//
// An outbox row committed independently of the booking it describes is the one
// bug this package exists to prevent — it would notify a student about a slot
// they may not have won. Non-negotiable #7 therefore holds by type-checking
// rather than by anyone remembering it at review time. Do not widen this to
// store.Querier: the pool satisfies that interface and the guarantee evaporates.
//
// Callers must already be inside store.WithTx. Nothing here sends anything;
// the row is a promise that the dispatcher keeps after the commit.
func Enqueue(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal %s: %w", topic, err)
	}

	// Sent as a string, not []byte. The pool runs in QueryExecModeExec (required
	// by transaction-mode pooling), where a []byte binds as bytea and the
	// ::jsonb cast then receives a hex-encoded blob rather than JSON.
	var id int64
	if err := tx.QueryRow(ctx, queries.Get(queries.OutboxInsert), topic, string(body)).Scan(&id); err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", topic, err)
	}
	return nil
}
