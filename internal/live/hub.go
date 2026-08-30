package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultBuffer is how many events one connection may fall behind by before it
// is dropped.
//
// Thirty-two is sized against the burst this system actually produces: the 6 PM
// rush is hundreds of attempts and exactly one confirmation per slot, so a
// client sees a handful of events per second, not hundreds. A connection that
// cannot absorb thirty-two of them is not slow, it is gone — a laptop that slept
// or a phone that walked out of wifi — and holding the buffer larger only delays
// noticing by making the publisher carry more memory per dead socket.
const DefaultBuffer = 32

// subscribeRetryDelay is how long the hub waits before re-dialling Redis.
//
// Short, and unbounded in attempts. There is nothing to give up on: the hub
// holds no state worth preserving, and every second it is not subscribed is a
// second of updates its clients simply do not get. They are not wrong meanwhile,
// only stale.
const subscribeRetryDelay = time.Second

// Subscription is one SSE connection's view of one date.
//
// Events() is closed exactly once, by whichever comes first: the handler calling
// Close on disconnect, the hub dropping this connection for falling behind, or
// the hub shutting down. A reader that sees the channel closed should end the
// response; a browser's EventSource then reconnects and refetches, which is the
// correct recovery from every one of those three causes.
type Subscription struct {
	hub  *Hub
	date string
	ch   chan Event

	closeOnce sync.Once
	dropped   atomic.Bool
}

// Events is the receive side. Never nil.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Date is the local date this subscription filters on.
func (s *Subscription) Date() string { return s.date }

// Dropped reports whether the hub closed this subscription because it could not
// keep up, as opposed to it being closed normally. Distinguishing them is worth
// one bool: the first is a client problem worth logging, the second is a browser
// tab closing.
func (s *Subscription) Dropped() bool { return s.dropped.Load() }

// Close deregisters and releases the subscription. Safe to call more than once,
// and safe to call concurrently with the hub dropping it.
func (s *Subscription) Close() {
	s.hub.remove(s)
	s.finish()
}

// finish closes the channel at most once. Only ever called after the
// subscription has been removed from the hub's map, so no send can be in flight.
func (s *Subscription) finish() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// Hub holds this replica's SSE connections and feeds them from Redis.
//
// One hub per process, one pattern subscription per hub, N subscriptions per
// hub. The replica-local half of §9's flow: the dispatcher publishes once, and
// every replica's hub delivers that one message to its own connected clients.
// No replica needs to know about any other, which is what keeps the API binary
// stateless and horizontally scalable.
type Hub struct {
	rdb    *redis.Client
	log    *slog.Logger
	buffer int

	// mu guards subs. Held only for non-blocking sends and map edits, never
	// across a network call — see dispatch.
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}

	ready     chan struct{}
	readyOnce sync.Once

	closed atomic.Bool
}

// NewHub builds the fan-out sink. A nil client is legal and yields a hub that
// delivers nothing: SSE clients connect, receive heartbeats, and fall back to
// polling — the documented degraded mode when Redis is unavailable.
func NewHub(rdb *redis.Client, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		rdb:    rdb,
		log:    log,
		buffer: DefaultBuffer,
		subs:   map[string]map[*Subscription]struct{}{},
		ready:  make(chan struct{}),
	}
}

// WithBuffer overrides the per-connection depth. Used by tests that need a
// consumer to fall behind on purpose.
func (h *Hub) WithBuffer(n int) *Hub {
	if n > 0 {
		h.buffer = n
	}
	return h
}

// Ready closes once the Redis subscription is live.
//
// It matters for the same reason the outbox dispatcher's does, but more sharply:
// Redis pub/sub has NO BACKLOG. A message published before the SUBSCRIBE is
// acknowledged is discarded, not queued. A test that publishes without waiting
// here is racing connection setup rather than testing fan-out.
//
// Never closes when the hub has no Redis client — there is no subscription to
// wait for.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Subscribe registers a connection interested in one local date.
//
// The returned Subscription must be Closed by the caller, normally with defer in
// the HTTP handler. Subscribe starts no goroutine: the handler's own goroutine
// does the reading, which is what makes a disconnected client cost nothing and
// leak nothing.
func (h *Hub) Subscribe(date string) *Subscription {
	s := &Subscription{
		hub:  h,
		date: date,
		ch:   make(chan Event, h.buffer),
	}

	h.mu.Lock()
	if h.subs[date] == nil {
		h.subs[date] = map[*Subscription]struct{}{}
	}
	h.subs[date][s] = struct{}{}
	n := len(h.subs[date])
	stopped := h.closed.Load()
	h.mu.Unlock()

	// Registered after the hub stopped: hand back a closed channel rather than
	// one nothing will ever write to, so the handler ends instead of hanging
	// until its own context expires.
	if stopped {
		h.remove(s)
		s.finish()
		return s
	}

	h.log.Debug("live subscriber joined", "date", date, "subscribers", n)
	return s
}

// remove deregisters a subscription. Idempotent.
func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	h.removeLocked(s)
	h.mu.Unlock()
}

func (h *Hub) removeLocked(s *Subscription) {
	set := h.subs[s.date]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) == 0 {
		// Drop the empty date bucket. Dates are unbounded over the life of a
		// process and a map that only grows is a slow leak.
		delete(h.subs, s.date)
	}
}

// Subscribers reports how many connections are watching a date. For tests and
// for the operational question "is anybody actually on this".
func (h *Hub) Subscribers(date string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[date])
}

// Dispatch delivers one event to every connection on THIS replica watching that
// date, without going through Redis.
//
// Exported because it is the hub's actual local-delivery contract, and the
// property that matters most about this package — that a stalled consumer
// cannot stall delivery for anybody else — is only testable here. Going through
// Redis to test it would measure the round trip instead of the fan-out.
//
// Not a shortcut for publishing: an event dispatched here reaches this replica's
// clients and no others. Everything the system emits goes through the outbox and
// Publisher so that every replica sees it.
//
// THE PUBLISHER NEVER WAITS. Every send is non-blocking, and a connection whose
// buffer is full is dropped rather than blocked on. That is the whole rule: this
// runs on the single goroutine draining the Redis subscription, so one stalled
// client blocking here would stall delivery for every other client on this
// replica — one dead phone would freeze the grid for everyone.
//
// Dropping rather than skipping the message is deliberate. A client that missed
// an event has an inconsistent grid and no way to know it; a client whose stream
// ended reconnects and refetches. Ending the connection is the honest signal.
func (h *Hub) Dispatch(date string, ev Event) {
	var dropped []*Subscription

	h.mu.Lock()
	for s := range h.subs[date] {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Store(true)
			dropped = append(dropped, s)
		}
	}
	for _, s := range dropped {
		h.removeLocked(s)
	}
	h.mu.Unlock()

	// Closed outside the lock, and only after removal, so nothing can be sending
	// on a channel being closed.
	for _, s := range dropped {
		s.finish()
	}

	if len(dropped) > 0 {
		h.log.Warn("dropped SSE subscribers that fell behind",
			"count", len(dropped), "date", date, "buffer", h.buffer)
	}
}

// Run subscribes to Redis and fans out until ctx is cancelled.
//
// Returns nil on cancellation. A dropped subscription is redialled rather than
// returned: losing the bus is a degradation, not a failure, and a hub that
// exited on the first blip would leave the replica permanently silent while
// still serving traffic.
func (h *Hub) Run(ctx context.Context) error {
	defer h.shutdown()

	if h.rdb == nil {
		h.log.Warn("live hub has no redis; SSE clients will receive heartbeats only")
		<-ctx.Done()
		return nil
	}

	for ctx.Err() == nil {
		err := h.runOnce(ctx)
		if err == nil || ctx.Err() != nil {
			continue
		}

		h.log.Warn("live hub subscription dropped; clients fall back to polling until it returns",
			"err", err, "retry_in", subscribeRetryDelay)

		select {
		case <-ctx.Done():
		case <-time.After(subscribeRetryDelay):
		}
	}
	return nil
}

// runOnce holds one pattern subscription and relays until it or ctx ends.
func (h *Hub) runOnce(ctx context.Context) error {
	ps := h.rdb.PSubscribe(ctx, ChannelPattern)
	defer func() { _ = ps.Close() }()

	// Receive blocks for the subscription confirmation. Without it the hub would
	// report ready before Redis had acknowledged anything, and the first events
	// after startup would be silently lost — see Ready.
	if _, err := ps.Receive(ctx); err != nil {
		return fmt.Errorf("live: subscribe %s: %w", ChannelPattern, err)
	}
	h.readyOnce.Do(func() { close(h.ready) })
	h.log.Info("live hub subscribed", "pattern", ChannelPattern)

	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return errors.New("live: subscription channel closed")
			}
			h.receive(msg.Channel, []byte(msg.Payload))
		}
	}
}

// receive decodes one message and hands it to the right date's subscribers.
//
// An unreadable payload is logged and dropped. It cannot be retried — pub/sub
// has no redelivery — and stopping the relay over one bad message would cost
// every well-formed message behind it.
func (h *Hub) receive(channel string, payload []byte) {
	date, ok := DateOf(channel)
	if !ok {
		return
	}

	var ev Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		h.log.Warn("live: unreadable event, ignoring", "channel", channel, "err", err)
		return
	}
	h.Dispatch(date, ev)
}

// shutdown closes every subscription so the handlers holding them return
// promptly instead of waiting on their own contexts.
func (h *Hub) shutdown() {
	h.closed.Store(true)

	h.mu.Lock()
	var all []*Subscription
	for _, set := range h.subs {
		for s := range set {
			all = append(all, s)
		}
	}
	h.subs = map[string]map[*Subscription]struct{}{}
	h.mu.Unlock()

	for _, s := range all {
		s.finish()
	}
	if len(all) > 0 {
		h.log.Info("live hub stopped; closed subscriptions", "count", len(all))
	}
}
