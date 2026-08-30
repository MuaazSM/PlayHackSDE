package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestOutboxNeverPrecedesCommit is the one that matters here.
//
// The claim non-negotiable #7 makes is not "we try to send after the commit".
// It is that a notification is PHYSICALLY INCAPABLE of preceding its commit,
// because the only thing that can wake a dispatcher is pg_notify, and pg_notify
// inside an AFTER INSERT trigger is delivered on commit — a rolled-back
// transaction emits nothing to be woken by, and leaves no row for the ticker to
// find either.
//
// So: enqueue, then force the transaction to fail. Assert the table is empty and
// the notifier was never called. Then commit one for real, to prove the first
// half was not passing simply because nothing worked.
func TestOutboxNeverPrecedesCommit(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	rec := newRecorder(nil)
	d := startDispatcher(t, pg, outbox.Options{
		Notifier:  rec,
		ListenDSN: pg.DSN,
		// Deliberately fast. If a rolled-back enqueue could ever be dispatched,
		// a 50ms ticker gives it a dozen chances inside this test.
		Interval: 50 * time.Millisecond,
	})
	awaitReady(t, d)

	boom := errors.New("caller failed after enqueueing")

	err := store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
			"booking_id": uuid.New(),
			"note":       "this booking never happened",
		}); err != nil {
			return err
		}
		// The INSERT has really run — the row exists inside this transaction and
		// its trigger has really queued a notification. Then the transaction
		// dies.
		return boom
	})
	require.ErrorIs(t, err, boom)

	// Several ticker periods, and enough wall time for a NOTIFY to have arrived
	// many times over had one been emitted.
	time.Sleep(500 * time.Millisecond)

	require.Zero(t, countOutbox(t, pg),
		"a rolled-back transaction left an outbox row behind")
	require.Zero(t, rec.count(),
		"the dispatcher sent a notification for a transaction that never committed")

	// Control: the same enqueue, committed, must arrive. Without this the two
	// assertions above would also pass on a dispatcher that does nothing at all.
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
			"booking_id": uuid.New(),
			"note":       "this one did",
		})
	}))

	waitFor(t, 5*time.Second, "the committed side effect to be delivered",
		func() bool { return rec.count() == 1 })
}

// TestDispatcher_DrainsOnNotify pins the latency claim: a committed side effect
// is dispatched on the commit, not on the next poll.
//
// The ticker is set to thirty seconds. Anything delivered inside this test was
// therefore delivered because of NOTIFY — the polling fallback could not have
// fired yet, which is what makes the measurement meaningful rather than a
// measurement of the ticker.
func TestDispatcher_DrainsOnNotify(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	rec := newRecorder(nil)
	d := startDispatcher(t, pg, outbox.Options{
		Notifier:  rec,
		ListenDSN: pg.DSN,
		Interval:  30 * time.Second,
	})
	awaitReady(t, d)

	committedAt := time.Now()
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicWaitlistPromoted, map[string]any{
			"entry_id": uuid.New(),
		})
	}))

	waitFor(t, 10*time.Second, "the notify-driven drain",
		func() bool { return rec.count() == 1 })

	latency := rec.deliveries()[0].at.Sub(committedAt)
	t.Logf("commit -> notification: %s", latency)
	require.Less(t, latency, 500*time.Millisecond,
		"delivery took %s; NOTIFY should deliver in tens of milliseconds, and the "+
			"ticker was 30s so this cannot be the polling path", latency)
}

// TestDispatcher_TickerCatchesMissedNotify covers the case NOTIFY cannot: a row
// that committed while nobody was listening.
//
// pg_notify is fire-and-forget. A dispatcher that was restarting, or whose
// listener connection had just dropped, never hears about the commits in that
// window. The rows are still there and still PENDING, which is why the outbox is
// a table and not a message queue — but without the ticker they would sit until
// the next booking happened to wake the dispatcher.
//
// Simulated by disabling the trigger, so the insert commits with no notification
// at all. That is a strictly harsher version of a missed notify.
func TestDispatcher_TickerCatchesMissedNotify(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	_, err := pg.Pool.Exec(ctx, `ALTER TABLE outbox DISABLE TRIGGER outbox_notify`)
	require.NoError(t, err)

	// Re-enable unconditionally. The container is shared across the package and
	// TRUNCATE does not restore a disabled trigger, so leaking this would break
	// every test that runs afterwards.
	t.Cleanup(func() {
		_, err := pg.Pool.Exec(context.Background(), `ALTER TABLE outbox ENABLE TRIGGER outbox_notify`)
		require.NoError(t, err)
	})

	rec := newRecorder(nil)
	d := startDispatcher(t, pg, outbox.Options{
		Notifier:  rec,
		ListenDSN: pg.DSN,
		Interval:  100 * time.Millisecond,
	})
	awaitReady(t, d)

	var id int64
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`INSERT INTO outbox (topic, payload) VALUES ($1, $2::jsonb) RETURNING id`,
		outbox.TopicBookingNoShow, `{"booking_id":"missed"}`).Scan(&id))

	waitFor(t, 10*time.Second, "the ticker to find the un-notified row",
		func() bool { return rec.count() == 1 })

	require.Equal(t, id, rec.deliveries()[0].msg.ID)
	require.Equal(t, "SENT", readRow(t, pg, id).Status)
}

// TestDispatcher_SkipLockedNoDoubleSend runs two dispatchers over one queue.
//
// This is the same construct as waitlist promotion, for the same reason: the
// claim is an UPDATE, and FOR UPDATE SKIP LOCKED means the second dispatcher
// takes the next un-locked batch instead of blocking on the first one's. Both
// halves matter — no row twice (correctness), and neither dispatcher idle behind
// the other (throughput).
func TestDispatcher_SkipLockedNoDoubleSend(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	const rows = 100

	// One transaction, so all hundred become visible at the same instant and the
	// two dispatchers genuinely contend rather than arriving in single file.
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		for i := 0; i < rows; i++ {
			if err := outbox.Enqueue(ctx, tx, outbox.TopicBookingConfirmed, map[string]any{
				"n": i,
			}); err != nil {
				return err
			}
		}
		return nil
	}))

	// Shared recorder: both dispatchers report into the same ledger, so a
	// duplicate send anywhere shows up as a repeated id.
	rec := newRecorder(nil)
	for i := 0; i < 2; i++ {
		startDispatcher(t, pg, outbox.Options{
			Notifier:  rec,
			ListenDSN: pg.DSN,
			Interval:  50 * time.Millisecond,
			// Small batches force many claims, so the two dispatchers interleave
			// instead of one taking everything in a single round trip.
			Batch: 10,
		})
	}

	waitFor(t, 20*time.Second, "all rows to be dispatched",
		func() bool { return rec.count() >= rows })

	// Give a straggler a chance to double-send before asserting there were none.
	time.Sleep(300 * time.Millisecond)

	seen := map[int64]int{}
	for _, id := range rec.ids() {
		seen[id]++
	}

	var duplicates []int64
	for id, n := range seen {
		if n > 1 {
			duplicates = append(duplicates, id)
		}
	}
	require.Empty(t, duplicates, "rows dispatched more than once: %v", duplicates)
	require.Len(t, seen, rows, "expected %d distinct rows dispatched", rows)
	require.Equal(t, rows, rec.count(), "total sends should equal distinct rows")
}

// TestDispatcher_RetriesFailedWithBackoff proves the retry is a real retry, and
// that it waits.
//
// A retry loop with no backoff is worse than no retry: a transport that is down
// gets hammered at full drain speed and the queue behind it never moves. The
// assertion is therefore on the GAP between the two attempts, not merely on the
// second attempt existing.
func TestDispatcher_RetriesFailedWithBackoff(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	const backoff = 500 * time.Millisecond

	// Fail the first attempt only. Attempts is stamped by the claim, so the
	// second delivery of the same row arrives with Attempts == 2.
	rec := newRecorder(func(m outbox.Message) error {
		if m.Attempts == 1 {
			return errors.New("transport down")
		}
		return nil
	})

	startDispatcher(t, pg, outbox.Options{
		Notifier:     rec,
		ListenDSN:    pg.DSN,
		Interval:     50 * time.Millisecond,
		MaxAttempts:  5,
		RetryBackoff: backoff,
	})

	var id int64
	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicBookingCancelled, map[string]any{
			"booking_id": uuid.New(),
		})
	}))
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT max(id) FROM outbox`).Scan(&id))

	waitFor(t, 20*time.Second, "the failed send to be retried",
		func() bool { return rec.count() >= 2 })

	got := rec.deliveries()
	require.Equal(t, id, got[0].msg.ID)
	require.Equal(t, id, got[1].msg.ID, "the retry must be the same row")
	require.Error(t, got[0].err)
	require.NoError(t, got[1].err)
	require.Equal(t, 1, got[0].msg.Attempts)
	require.Equal(t, 2, got[1].msg.Attempts, "attempts must carry across the retry")

	gap := got[1].at.Sub(got[0].at)
	t.Logf("failure -> retry: %s (backoff %s, tick 50ms)", gap, backoff)
	require.GreaterOrEqual(t, gap, backoff-100*time.Millisecond,
		"the retry came after %s, which is faster than the %s backoff — a failing "+
			"transport would be retried at drain speed", gap, backoff)

	// And it converges: the row ends SENT, not stuck in FAILED.
	waitFor(t, 5*time.Second, "the row to settle as SENT",
		func() bool { return readRow(t, pg, id).Status == "SENT" })
	require.Equal(t, 2, readRow(t, pg, id).Attempts)
}

// TestDispatcher_MarksFailedAfterMaxAttempts pins the other end of at-least-once.
//
// "At least once" is a promise about retrying, not about retrying forever. A row
// that has exhausted its attempts stays FAILED and stops consuming drain
// capacity — a dead-letter queue that happens to be a WHERE clause on the same
// table, which is a better operational story than a poison message quietly
// starving every notification behind it.
func TestDispatcher_MarksFailedAfterMaxAttempts(t *testing.T) {
	pg := testutil.Postgres(t)
	ctx := context.Background()

	const maxAttempts = 2

	rec := newRecorder(func(outbox.Message) error {
		return errors.New("transport permanently down")
	})

	startDispatcher(t, pg, outbox.Options{
		Notifier:     rec,
		ListenDSN:    pg.DSN,
		Interval:     30 * time.Millisecond,
		MaxAttempts:  maxAttempts,
		RetryBackoff: 100 * time.Millisecond,
	})

	require.NoError(t, store.WithTx(ctx, pg.Pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.TopicClosureCreated, map[string]any{
			"facility_id": uuid.New(),
		})
	}))

	var id int64
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT max(id) FROM outbox`).Scan(&id))

	waitFor(t, 20*time.Second, "attempts to reach the ceiling",
		func() bool { return readRow(t, pg, id).Attempts >= maxAttempts })

	// Well past several backoff periods and dozens of ticks. If the ceiling were
	// not enforced, attempts would keep climbing here.
	time.Sleep(time.Second)

	final := readRow(t, pg, id)
	require.Equal(t, "FAILED", final.Status,
		"an exhausted row must stay FAILED so it is visible to an operator")
	require.Equal(t, maxAttempts, final.Attempts,
		"the row was retried past MaxAttempts")
	require.Equal(t, maxAttempts, rec.count(),
		"the transport was called more times than the attempt ceiling allows")
}
