// Package live is the fan-out half of IMPLEMENTATION.md §9: a state transition
// leaves the outbox dispatcher, crosses Redis pub/sub, and arrives at every SSE
// client connected to any API replica.
//
// REDIS IS A BUS HERE, NOT A STORE (non-negotiable #3). Nothing in this package
// is consulted before a booking is decided, and nothing on the write path reads
// what it publishes. Wipe Redis mid-run and the only effect is that live updates
// stop arriving until the next grid fetch — the exclusion constraint carries on
// deciding races exactly as it did before.
//
// That is also why the publisher's errors are returned for logging rather than
// for retrying. A lost live update costs a client one stale cell for a few
// seconds; there is nothing here worth failing a drain over.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/outbox"
	"github.com/redis/go-redis/v9"
)

// Channel naming. One channel per local date, so a client watching Tuesday is
// not woken by every booking made for Wednesday — the filter runs in Redis
// rather than in each of N replicas.
const (
	ChannelPrefix = "slots:"

	// ChannelPattern is what a hub subscribes to. One pattern subscription for
	// the whole process, rather than one per date: dates come and go as clients
	// connect, and re-SUBSCRIBEing on every connection would drop messages in
	// the window between the old subscription ending and the new one landing.
	ChannelPattern = ChannelPrefix + "*"
)

// Layouts. The date must match the availability cache key and the ?date= query
// parameter exactly, or the invalidation would clear a key nobody reads.
const (
	dateLayout = "2006-01-02"
	slotLayout = "15:04"
)

// Channel is the one place a channel name is spelled.
func Channel(date string) string { return ChannelPrefix + date }

// DateOf recovers the local date from a channel name. Reports false for a
// channel that is not ours, which a pattern subscription can legitimately see.
func DateOf(channel string) (string, bool) {
	if !strings.HasPrefix(channel, ChannelPrefix) {
		return "", false
	}
	date := strings.TrimPrefix(channel, ChannelPrefix)
	if date == "" {
		return "", false
	}
	return date, true
}

// Event is the payload, and it is deliberately tiny (§9).
//
// Three fields, no booking id, no user id, no reason. This crosses a public
// fan-out channel that every connected student receives, so it carries only what
// the grid already shows in public. It is also a HINT rather than a record: the
// same transition invalidates the campus cache, and the grid — derived from the
// bookings table at read time, per non-negotiable #4 — remains the only thing
// that is actually true.
type Event struct {
	FacilityID uuid.UUID `json:"facility_id"`
	Slot       string    `json:"slot"`
	State      string    `json:"state"`
}

// StateFor maps an outbox topic onto the grid state a transition leaves behind,
// and reports false for topics that are not about occupancy.
//
// The vocabulary is facility's, not a second set of strings: a client switches
// on the same states the availability grid renders, so a live patch and a
// refetch agree about what "held" means.
//
// The mapping is complete for every topic the product names, including the ones
// whose producer is not built yet (no-show is Phase 11, closures are Phase 12).
// A topic that arrives before its feature does is published correctly rather
// than silently dropped, and the alternative — adding the case at the same time
// as the producer — is how one of them gets forgotten.
func StateFor(topic string) (string, bool) {
	switch topic {
	case outbox.TopicBookingConfirmed:
		return facility.StateBooked, true

	case outbox.TopicWaitlistPromoted:
		// A hold reserves the window for one student for the claim period. It is
		// not free and it is not theirs yet.
		return facility.StateHeld, true

	case outbox.TopicBookingCancelled, outbox.TopicWaitlistExpired, outbox.TopicBookingNoShow:
		// All three release a window. "free" is optimistic for a SHARED facility,
		// whose real state after a release may still be filling or full, and for
		// an exclusive window that is re-offered to the queue in the same
		// transaction. Both self-correct on the refetch this event triggers —
		// see Publisher.Publish, which invalidates the grid before it publishes.
		return facility.StateFree, true

	case outbox.TopicClosureCreated:
		return facility.StateClosed, true

	default:
		// booking.reminder and anything added later. Not an occupancy change.
		return "", false
	}
}

// transition is the subset of an outbox payload this package needs.
//
// Every occupancy topic carries facility_id and start, because every one of them
// is about a window on a facility. Decoding just those two keeps this decoupled
// from the rest of each payload's shape.
type transition struct {
	FacilityID uuid.UUID `json:"facility_id"`
	Start      time.Time `json:"start"`
}

// Publisher turns drained outbox rows into live events.
//
// It satisfies outbox.SlotPublisher. It is attached to a Dispatcher, so it runs
// AFTER the claim has committed — a live update, like a notification, cannot
// describe a booking that rolled back.
type Publisher struct {
	rdb *redis.Client
	loc *time.Location
	log *slog.Logger
}

// NewPublisher builds the fan-out source.
//
// loc is the campus timezone, and it is required rather than defaulted quietly:
// the date it produces is both the channel name and the cache key, so a
// publisher on UTC and a reader on IST would disagree about which day a 23:30
// booking belongs to and the update would be delivered to nobody.
func NewPublisher(rdb *redis.Client, loc *time.Location, log *slog.Logger) *Publisher {
	if loc == nil {
		loc = time.UTC
	}
	if log == nil {
		log = slog.Default()
	}
	return &Publisher{rdb: rdb, loc: loc, log: log}
}

// PublishTransition implements outbox.SlotPublisher.
//
// A topic with no occupancy meaning is not an error — it returns nil, having
// done nothing.
func (p *Publisher) PublishTransition(ctx context.Context, topic string, payload json.RawMessage) error {
	state, ok := StateFor(topic)
	if !ok {
		return nil
	}

	var t transition
	if err := json.Unmarshal(payload, &t); err != nil {
		return fmt.Errorf("live: decode %s payload: %w", topic, err)
	}
	if t.FacilityID == uuid.Nil || t.Start.IsZero() {
		return fmt.Errorf("live: %s payload carries no facility_id and start", topic)
	}

	// Localised here and nowhere else. The row holds timestamptz in UTC per the
	// Time convention; IST is applied at this edge, exactly as it is at the HTTP
	// edge, so "18:00" means what a student reads on the grid.
	local := t.Start.In(p.loc)

	return p.Publish(ctx, local.Format(dateLayout), Event{
		FacilityID: t.FacilityID,
		Slot:       local.Format(slotLayout),
		State:      state,
	})
}

// Publish invalidates the date's cached grid and announces the change.
//
// ORDER IS LOAD-BEARING: the DEL goes before the PUBLISH. A client that receives
// the event refetches the grid immediately, and if the invalidation landed after
// the announcement that refetch could re-warm the cache from the very entry the
// event said was stale — which would then survive its full TTL. Doing it the
// other way round costs nothing: a DEL of a cold key is a no-op.
//
// One pipeline, so both land in one round trip and in that order.
func (p *Publisher) Publish(ctx context.Context, date string, ev Event) error {
	// No Redis is a supported configuration, not a failure. The service runs
	// without it; only the live half goes quiet.
	if p.rdb == nil {
		return nil
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("live: marshal event: %w", err)
	}

	pipe := p.rdb.Pipeline()
	pipe.Del(ctx, facility.CampusCacheKey(date))
	pipe.Publish(ctx, Channel(date), raw)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("live: publish %s: %w", Channel(date), err)
	}

	p.log.DebugContext(ctx, "live slot update published",
		"date", date, "facility_id", ev.FacilityID, "slot", ev.Slot, "state", ev.State)
	return nil
}
