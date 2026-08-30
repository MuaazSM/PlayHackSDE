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

	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
)

// Topics published by the write path.
const (
	TopicBookingConfirmed = "booking.confirmed"
	TopicBookingCancelled = "booking.cancelled"
)

// Enqueue writes a side effect inside the caller's transaction.
//
// It takes a store.Querier rather than a pool precisely so it CANNOT be called
// outside one: an outbox row committed independently of its booking is the bug
// this package exists to prevent.
func Enqueue(ctx context.Context, q store.Querier, topic string, payload any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("outbox: marshal %s: %w", topic, err)
	}

	// Sent as a string, not []byte. The pool runs in QueryExecModeExec (required
	// by transaction-mode pooling), where a []byte binds as bytea and the
	// ::jsonb cast then receives a hex-encoded blob rather than JSON.
	var id int64
	if err := q.QueryRow(ctx, queries.Get(queries.OutboxInsert), topic, string(body)).Scan(&id); err != nil {
		return 0, fmt.Errorf("outbox: enqueue %s: %w", topic, err)
	}
	return id, nil
}
