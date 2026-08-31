package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/observability"
	"github.com/iitg-playhack/sportsbook/internal/store"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
)

// Defaults for a Dispatcher. All overridable through Options.
const (
	// DefaultInterval is the safety-net tick (§8). NOTIFY does the real work;
	// this only exists to cover the windows NOTIFY cannot: rows committed while
	// the listener was reconnecting, and rows left PENDING by a process that
	// died between the commit and the send.
	DefaultInterval = 5 * time.Second

	// DefaultBatch is the §8 claim size. Bounds how much work one transaction
	// holds locked, and how many notifications a crash can re-deliver.
	DefaultBatch = 50

	// DefaultMaxAttempts is the dead-letter boundary. Five attempts across the
	// backoff schedule below is a little over four minutes of trying, after
	// which a row is a real problem rather than a blip.
	DefaultMaxAttempts = 5

	// DefaultRetryBackoff is the base of the exponential: 2s, 4s, 8s, 16s.
	DefaultRetryBackoff = 2 * time.Second

	// DefaultMaxBackoff caps the exponential so a long outage does not push the
	// next attempt hours away.
	DefaultMaxBackoff = 5 * time.Minute

	// maxRoundsPerPass bounds one wake-up. A backlog larger than this is drained
	// by the next tick rather than by one goroutine spinning through it, so a
	// dispatcher catching up after an outage still responds to shutdown.
	maxRoundsPerPass = 20

	// listenRetryDelay is how long the listener waits before redialling. Short,
	// because the ticker is covering the gap and nothing is lost meanwhile.
	listenRetryDelay = time.Second
)

// SlotPublisher is the live-update seam (§9).
//
// The dispatcher hands it every drained row and takes back an error it only
// logs. The asymmetry with Notifier is the point: a notification that fails to
// send is retried, a live update that fails to publish is forgotten. Redis is a
// BUS here and never authoritative (non-negotiable #3), so a publish that does
// not land costs a client one stale grid cell until its next fetch, and costs
// correctness nothing at all. Retrying it would be strictly worse — the event is
// about a moment that has already passed.
//
// Deliberately (topic, payload) rather than Message: the dispatcher stays
// topic-agnostic, exactly as Message documents, and the mapping from a topic to
// a slot state lives with the package that owns the grid's vocabulary.
type SlotPublisher interface {
	PublishTransition(ctx context.Context, topic string, payload json.RawMessage) error
}

// Options configures a Dispatcher. The zero value of every field falls back to
// the Default* constants above.
type Options struct {
	// Notifier is the transport. Required.
	Notifier Notifier

	// SlotPublisher fans state transitions out to connected SSE clients (§9).
	// Optional: left nil, the dispatcher publishes nothing and the system is
	// exactly as correct, with clients falling back to polling.
	SlotPublisher SlotPublisher

	// ListenDSN is a DIRECT Postgres connection string for the LISTEN session —
	// not the PgBouncer address. LISTEN is session state and transaction-mode
	// pooling hands the backend to another client between transactions, so a
	// subscription taken through the pooler would be silently dropped.
	//
	// Empty degrades the dispatcher to ticker-only: still correct, up to
	// Interval slower. That is the honest fallback rather than a boot failure —
	// the outbox is a liveness mechanism, not a correctness one.
	ListenDSN string

	Interval     time.Duration
	Batch        int
	MaxAttempts  int
	RetryBackoff time.Duration
	MaxBackoff   time.Duration

	Logger *slog.Logger
}

// Dispatcher drains the outbox and sends what it finds.
//
// It is the second half of non-negotiable #7. The first half is Enqueue, which
// cannot write outside a transaction; this half cannot see a row until that
// transaction commits. Between them there is no arrangement of failures that
// produces a notification for a booking that did not happen.
//
// Several instances may run at once. The claim in outbox_drain.sql uses
// FOR UPDATE SKIP LOCKED, so two dispatchers take disjoint batches rather than
// the same one — the same construct, and the same reason, as waitlist promotion.
type Dispatcher struct {
	db       *store.DB
	notifier Notifier
	slots    SlotPublisher
	log      *slog.Logger

	listenDSN    string
	interval     time.Duration
	batch        int
	maxAttempts  int
	retryBackoff time.Duration
	maxBackoff   time.Duration

	// ready closes once LISTEN has been accepted, so a caller can know the
	// subscription is live before relying on notify-speed delivery.
	ready     chan struct{}
	readyOnce sync.Once
}

// NewDispatcher builds a dispatcher over db's primary pool.
//
// Draining is a write, so it goes to the primary. A replica cannot serve it: the
// claim is an UPDATE, and reading candidate rows from a replica would hand the
// same row to two dispatchers the moment replication lagged.
func NewDispatcher(db *store.DB, opt Options) *Dispatcher {
	d := &Dispatcher{
		db:           db,
		notifier:     opt.Notifier,
		slots:        opt.SlotPublisher,
		log:          opt.Logger,
		listenDSN:    opt.ListenDSN,
		interval:     opt.Interval,
		batch:        opt.Batch,
		maxAttempts:  opt.MaxAttempts,
		retryBackoff: opt.RetryBackoff,
		maxBackoff:   opt.MaxBackoff,
		ready:        make(chan struct{}),
	}

	if d.notifier == nil {
		d.notifier = NewLogNotifier(opt.Logger)
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.interval <= 0 {
		d.interval = DefaultInterval
	}
	if d.batch <= 0 {
		d.batch = DefaultBatch
	}
	if d.maxAttempts <= 0 {
		d.maxAttempts = DefaultMaxAttempts
	}
	if d.retryBackoff <= 0 {
		d.retryBackoff = DefaultRetryBackoff
	}
	if d.maxBackoff <= 0 {
		d.maxBackoff = DefaultMaxBackoff
	}
	return d
}

// WithSlotPublisher attaches the live-update fan-out after construction.
//
// Exists because NewFromConfig builds the dispatcher from configuration alone
// while the publisher needs a dialled Redis client and the campus timezone,
// which the binaries own. Widening NewFromConfig to take both would make every
// caller supply a Redis client for a feature that is optional by design.
func (d *Dispatcher) WithSlotPublisher(p SlotPublisher) *Dispatcher {
	d.slots = p
	return d
}

// Ready is closed once the LISTEN subscription is established. It never closes
// when ListenDSN is empty — in ticker-only mode there is no subscription to wait
// for.
func (d *Dispatcher) Ready() <-chan struct{} { return d.ready }

// Run drains until ctx is cancelled.
//
// Two wake-up sources, and the asymmetry between them is the design:
//
//   - NOTIFY, which pg_notify fires ON COMMIT — not on statement. That single
//     property is the whole trick. The dispatcher is woken by the commit itself,
//     so it learns about a booking at the instant the booking becomes true, and
//     a rolled-back transaction produces no notification to be woken by. It is
//     also why this is not Kafka: the database already has an ordered,
//     transactional, commit-triggered log and a way to subscribe to it.
//
//   - A ticker, as a safety net only. NOTIFY is fire-and-forget: a listener that
//     was reconnecting when the commit landed never hears about it, and a
//     process that died after claiming a batch leaves rows nobody will be
//     notified about. Neither is a correctness problem — the rows are still
//     there — but without the tick they would sit until the next booking on the
//     same instance. Five seconds is the bound on that.
func (d *Dispatcher) Run(ctx context.Context) error {
	// Buffered depth 1, and sends are non-blocking. Notifications coalesce: a
	// burst of five hundred commits should produce one drain that finds five
	// hundred rows, not five hundred drains.
	wake := make(chan struct{}, 1)

	var wg sync.WaitGroup
	if d.listenDSN != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.listen(ctx, wake)
		}()
	} else {
		d.log.Warn("outbox dispatcher has no listen DSN; falling back to polling",
			"interval", d.interval)
	}

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	d.log.Info("outbox dispatcher started",
		"interval", d.interval, "batch", d.batch,
		"max_attempts", d.maxAttempts, "listening", d.listenDSN != "")

	// Catch up on anything left behind by a previous process before waiting.
	d.pass(ctx)

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			d.log.Info("outbox dispatcher stopped")
			return nil
		case <-wake:
			d.pass(ctx)
		case <-ticker.C:
			d.pass(ctx)
		}
	}
}

// listen holds a dedicated connection subscribed to outbox_new and pokes wake on
// every notification.
//
// Dedicated because LISTEN is session state: it cannot share the pgx pool, which
// points at PgBouncer in transaction mode and would hand the backend away.
//
// A dropped connection is redialled, not fatal. The ticker covers the gap, so
// losing the subscription costs latency and nothing else.
func (d *Dispatcher) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := d.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			d.log.Warn("outbox listener dropped; falling back to the ticker until it reconnects",
				"err", err, "retry_in", listenRetryDelay)

			select {
			case <-ctx.Done():
				return
			case <-time.After(listenRetryDelay):
			}
		}
	}
}

// listenOnce dials, subscribes, and blocks relaying notifications until the
// connection or the context ends.
func (d *Dispatcher) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := pgx.Connect(ctx, d.listenDSN)
	if err != nil {
		return fmt.Errorf("outbox: listen dial: %w", err)
	}
	defer func() {
		// Background context: ctx is usually already cancelled by the time we
		// unwind, and the connection still needs closing.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, queries.Get(queries.OutboxListen)); err != nil {
		return fmt.Errorf("outbox: listen: %w", err)
	}

	// Anything committed between the last drain and this subscription would be
	// missed by NOTIFY, so sweep once now rather than waiting for the ticker.
	d.pass(ctx)
	d.readyOnce.Do(func() { close(d.ready) })
	d.log.Info("outbox listener subscribed", "channel", "outbox_new")

	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("outbox: wait for notification: %w", err)
		}

		// Non-blocking: a full buffer already means "there is work", and the
		// drain that answers it will see this row too.
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// pass drains everything currently claimable, in batches.
//
// Failed rows are returned to the queue first so a retry rides the same
// round-trips as new work. A failure here is logged and dropped: the rows are
// still PENDING, the next tick finds exactly the same work, and stopping the
// loop would turn a database blip into a permanently silent system.
func (d *Dispatcher) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	if n, err := d.requeueFailed(ctx); err != nil {
		if ctx.Err() == nil {
			d.log.Error("outbox requeue failed; retrying next tick", "err", err)
		}
	} else if n > 0 {
		d.log.Info("outbox rows returned to the queue for retry", "count", n)
	}

	for round := 0; round < maxRoundsPerPass; round++ {
		n, err := d.drainOnce(ctx)
		if err != nil {
			if ctx.Err() == nil {
				d.log.Error("outbox drain failed; retrying next tick", "err", err)
			}
			return
		}
		// A short batch means the queue is empty. A full one means there is
		// probably more behind it.
		if n < d.batch {
			d.sampleBacklog(ctx)
			return
		}
	}
	d.sampleBacklog(ctx)
}

// sampleBacklog publishes outbox_pending (§14).
//
// Once per pass, at the END of it, so the gauge reads what is still waiting
// rather than what arrived. Best effort and never fatal: this is a dashboard
// input, and a dispatcher that stopped draining because it could not count is
// the failure the gauge exists to reveal.
func (d *Dispatcher) sampleBacklog(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var pending int64
	if err := d.db.Primary.QueryRow(ctx, queries.Get(queries.OutboxPending)).Scan(&pending); err != nil {
		d.log.Debug("outbox backlog sample failed", "err", err)
		return
	}
	observability.SetOutboxPending(pending)
}

// drainOnce claims one batch, commits, and only then sends.
//
// The ordering is the rule from CLAUDE.md's Conventions: no network calls inside
// a transaction. It is also what makes the send safe to be slow — a transport
// timing out holds no database locks and blocks no booking.
//
// The claim is optimistic: rows are marked SENT before the transport confirms.
// A process that dies between the commit and the send leaves them SENT and
// undelivered. That is the deliberate trade — the alternative holds the batch
// PENDING across a network call, and a duplicate notification is cheaper than a
// dispatcher that stalls the queue whenever a push endpoint hangs.
func (d *Dispatcher) drainOnce(ctx context.Context) (int, error) {
	var msgs []Message

	err := store.WithTx(ctx, d.db.Primary, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, queries.Get(queries.OutboxDrain), d.batch)
		if err != nil {
			return fmt.Errorf("outbox: drain: %w", store.Classify(err))
		}
		defer rows.Close()

		for rows.Next() {
			var (
				m       Message
				payload []byte
			)
			if err := rows.Scan(&m.ID, &m.Topic, &payload, &m.Attempts); err != nil {
				return fmt.Errorf("outbox: drain: scan: %w", err)
			}
			m.Payload = json.RawMessage(payload)
			msgs = append(msgs, m)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("outbox: drain: %w", store.Classify(err))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	// ---- past this line the claim is committed; sends may take as long as they
	// like without holding anything.

	var failed []int64
	for _, m := range msgs {
		// Live updates go first. They are what somebody is watching a screen
		// for, and they must not queue behind a transport that is timing out.
		d.publishSlot(ctx, m)

		if err := d.notifier.Notify(ctx, m); err != nil {
			d.log.Warn("notification send failed",
				"outbox_id", m.ID, "topic", m.Topic, "attempt", m.Attempts, "err", err)
			failed = append(failed, m.ID)
			continue
		}
		d.log.Debug("notification sent",
			"outbox_id", m.ID, "topic", m.Topic, "attempt", m.Attempts)
	}

	if len(failed) > 0 {
		// A cancelled context is why most of these failed during shutdown; mark
		// them on a fresh one so they are not left claimed-but-unrecorded.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		if err := d.markFailed(markCtx, failed); err != nil {
			// The rows stay SENT. Logged loudly because this is the one path
			// that genuinely loses a notification.
			d.log.Error("could not mark notifications failed; they will not be retried",
				"count", len(failed), "err", err)
		}
	}

	return len(msgs), nil
}

// publishSlot fans one transition out to connected clients.
//
// Best effort by construction. An error is logged and dropped: it is never
// retried, and it NEVER contributes to the failed set — a Redis blip must not
// mark an outbox row FAILED and re-deliver a notification that was already sent
// perfectly well. The two side effects share a row and share nothing else.
func (d *Dispatcher) publishSlot(ctx context.Context, m Message) {
	if d.slots == nil {
		return
	}
	if err := d.slots.PublishTransition(ctx, m.Topic, m.Payload); err != nil {
		d.log.WarnContext(ctx, "live slot update not published; clients will see it on their next fetch",
			"outbox_id", m.ID, "topic", m.Topic, "err", err)
	}
}

// markFailed records the verdict for sends that did not go through.
func (d *Dispatcher) markFailed(ctx context.Context, ids []int64) error {
	_, err := d.db.Primary.Exec(ctx, queries.Get(queries.OutboxMarkFailed), ids)
	if err != nil {
		return fmt.Errorf("outbox: mark failed: %w", store.Classify(err))
	}
	return nil
}

// requeueFailed returns due FAILED rows to PENDING and reports how many.
//
// Rows past MaxAttempts are left alone. They are a dead-letter queue of one
// table: visible to `SELECT * FROM outbox WHERE status = 'FAILED'`, which is a
// better operational story than a retry loop that never gives up.
func (d *Dispatcher) requeueFailed(ctx context.Context) (int, error) {
	rows, err := d.db.Primary.Query(ctx, queries.Get(queries.OutboxRequeueFailed),
		d.maxAttempts,
		d.retryBackoff.Seconds(),
		d.maxBackoff.Seconds(),
		d.batch,
	)
	if err != nil {
		return 0, fmt.Errorf("outbox: requeue: %w", store.Classify(err))
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox: requeue: %w", store.Classify(err))
	}
	return n, nil
}

// DrainNow runs a single pass synchronously. It exists for tests and for the
// race console's reset path, which want the queue settled before asserting
// against it rather than at some point in the next five seconds.
//
// Nothing on a request path may call this: a handler that drains the outbox is
// a handler that sends notifications, which is exactly what #7 forbids.
func (d *Dispatcher) DrainNow(ctx context.Context) error {
	if _, err := d.requeueFailed(ctx); err != nil {
		return err
	}
	for round := 0; round < maxRoundsPerPass; round++ {
		n, err := d.drainOnce(ctx)
		if err != nil {
			return err
		}
		if n < d.batch {
			d.sampleBacklog(ctx)
			return nil
		}
	}
	return errors.New("outbox: drain did not settle; backlog exceeds one pass")
}
