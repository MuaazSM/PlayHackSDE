package waitlist

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// SweepInterval is how often expired promotion offers are reclaimed (§6.3).
//
// Thirty seconds is a deliberate choice about who pays for the delay. The
// promoted student already had PROMOTION_TTL_MIN; the next student in line
// waits up to half a minute longer than strictly necessary. Sweeping every
// second would spend a transaction per second on a queue that is empty almost
// all day, to save a wait nobody is watching a stopwatch for.
const SweepInterval = 30 * time.Second

// sweepBatch bounds one pass. A pass holds row locks for its duration, so an
// unbounded batch after an outage would be one long transaction blocking live
// claims. Whatever is left over is picked up thirty seconds later.
const sweepBatch = 100

// SweepResult is what one pass did.
type SweepResult struct {
	// Expired is the number of unclaimed holds returned to the pool.
	Expired int
	// Promoted is the number of those windows immediately offered to the next
	// student in line.
	Promoted int
}

// Typed nils and constants for the audit trail. booking_events.from_status is
// an enum and actor_id a uuid; an untyped nil would leave the driver to guess
// which.
var (
	noActor    *uuid.UUID
	statusHeld = "HELD"
)

// expiredHold is one reclaimed offer.
type expiredHold struct {
	bookingID  uuid.UUID
	facilityID uuid.UUID
	userID     uuid.UUID
	start      time.Time
	end        time.Time
}

// Sweep runs one pass: every HELD booking whose claim window has closed goes to
// CANCELLED, its queue entry to EXPIRED, and the freed window is offered to the
// next student in line. IMPLEMENTATION.md §6.3.
//
// The promotion goes through the SAME waitlist_claim_head statement a live
// cancel uses, and that is the point rather than code reuse. Had the sweeper
// grown its own "find the next student" SELECT, the two would be independent
// readers of the same rows: both would read the head, both would promote it,
// and one student would be handed two courts. Claiming through the one locking
// statement is what makes that impossible — and SKIP LOCKED inside it is what
// stops the sweeper's batch-long transaction blocking every live cancel on that
// facility while it runs.
//
// One transaction per pass. The expiry and the promotion it triggers commit
// together, so there is never a moment where a court has been released and
// nobody has been offered it.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult

	err := store.WithTx(ctx, s.db.Primary, func(tx pgx.Tx) error {
		expired, err := s.reclaimExpired(ctx, tx)
		if err != nil {
			return err
		}
		res.Expired = len(expired)

		for _, h := range expired {
			// The queue entry retires with the hold it pointed at.
			if _, err := tx.Exec(ctx, queries.Get(queries.WaitlistMarkExpired), h.bookingID); err != nil {
				return fmt.Errorf("waitlist: mark expired: %w", store.Classify(err))
			}

			// actor is NULL: nobody did this, a clock did. Both NULLs are typed,
			// so the driver encodes them rather than guessing at a column type.
			if _, err := tx.Exec(ctx, queries.Get(queries.BookingEventInsert),
				h.bookingID, noActor, &statusHeld, "CANCELLED", "promotion offer expired"); err != nil {
				return fmt.Errorf("waitlist: expiry event: %w", store.Classify(err))
			}

			// The released window, announced to anyone watching the grid (§9).
			//
			// Enqueued BEFORE the promotion below, and the order matters: outbox
			// rows drain by (created_at, id), and every row in one transaction
			// shares a created_at because now() is transaction time. The id is
			// therefore the tiebreaker, so inserting here first is what makes a
			// re-offered window publish free-then-held rather than the reverse
			// and leave every grid showing a slot that is not actually free.
			//
			// Inside the sweep transaction, per non-negotiable #7: an expiry that
			// rolls back takes this row with it.
			if err := outbox.Enqueue(ctx, tx, outbox.TopicWaitlistExpired, map[string]any{
				"booking_id":  h.bookingID,
				"facility_id": h.facilityID,
				"user_id":     h.userID,
				"start":       h.start,
				"end":         h.end,
			}); err != nil {
				return store.Classify(err)
			}

			promotion, err := s.promote(ctx, tx, h.facilityID, h.start, h.end)
			if err != nil {
				return err
			}
			if promotion != nil {
				res.Promoted++
			}
		}
		return nil
	})
	if err != nil {
		return SweepResult{}, err
	}

	if res.Expired > 0 {
		s.log.InfoContext(ctx, "waitlist sweep",
			"expired", res.Expired, "promoted", res.Promoted)
	}
	return res, nil
}

// reclaimExpired runs the batch UPDATE and drains the result set completely
// before returning.
//
// Draining first is not tidiness: pgx runs one query at a time per connection,
// and the caller issues more statements on this transaction for every row here.
// Holding the rows open while doing that would fail on the first of them.
func (s *Service) reclaimExpired(ctx context.Context, tx pgx.Tx) ([]expiredHold, error) {
	rows, err := tx.Query(ctx, queries.Get(queries.BookingExpireHeld), sweepBatch)
	if err != nil {
		return nil, fmt.Errorf("waitlist: expire holds: %w", store.Classify(err))
	}
	defer rows.Close()

	var out []expiredHold
	for rows.Next() {
		var h expiredHold
		if err := rows.Scan(&h.bookingID, &h.facilityID, &h.userID, &h.start, &h.end); err != nil {
			return nil, fmt.Errorf("waitlist: expire holds: scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("waitlist: expire holds: %w", store.Classify(err))
	}
	return out, nil
}

// RunSweeper sweeps every interval until ctx is cancelled.
//
// A failed pass is logged and the next one is scheduled. There is nothing to
// recover: the rows it did not reclaim are still HELD and still expired, so the
// next pass finds exactly the same work. Stopping the loop on an error would
// turn a transient database blip into a permanently stuck queue.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = SweepInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log.Info("waitlist sweeper started", "interval", interval, "claim_window", s.ttl)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("waitlist sweeper stopped")
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("waitlist sweep failed; retrying next tick", "err", err)
			}
		}
	}
}
