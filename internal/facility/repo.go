// Package facility is the venue catalogue: what exists, when it opens, how long
// a booking may run.
//
// It caches facility rows in-process for 60s. That is a cache of CONFIGURATION,
// not of occupancy — availability is always derived from the bookings table at
// read time. Non-negotiable #3 is untouched: nothing here is ever consulted to
// decide whether a slot is free.
package facility

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means no facility exists with the requested id.
var ErrNotFound = errors.New("facility not found")

// DefaultTTL is how long a cached facility row is trusted. The catalogue is
// small and changes about never; 60s bounds the staleness a manager sees after
// editing opening hours.
const DefaultTTL = 60 * time.Second

// Facility is a bookable venue.
//
// OpensAt and ClosesAt are offsets from local midnight rather than wall-clock
// times, because that is the only form in which "is this window inside opening
// hours" is a comparison and not a date-arithmetic bug waiting to happen.
type Facility struct {
	ID          uuid.UUID
	Name        string
	Sport       string
	IsExclusive bool
	Capacity    int
	OpensAt     time.Duration
	ClosesAt    time.Duration
	Granularity time.Duration
	MinDuration time.Duration
	MaxDuration time.Duration
	IsActive    bool
}

type entry struct {
	facility Facility
	expires  time.Time
}

// Repo reads the catalogue, with a TTL cache in front.
//
// CONSTRAINT ON ANY REWRITE OF THIS TYPE: Get is called from INSIDE a booking
// transaction (booking.Cancel needs the facility to decide whether to release a
// capacity counter). The conventions forbid network calls inside a transaction,
// and this only complies because a cache miss is a local Postgres read on a
// pooled connection — fast, and against the same server the transaction already
// holds.
//
// Backing this cache with Redis, or any other out-of-process store, would put a
// network round trip inside a transaction holding locks and would violate that
// rule invisibly. If a shared cache is ever wanted, either keep an in-process
// tier in front of it, or hoist the lookup out of the transaction and pass the
// facility in.
//
// The pub/sub invalidation hook (Invalidate) is fine: it is a push, and it does
// not run on the transaction's path.
type Repo struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time

	mu    sync.RWMutex
	cache map[uuid.UUID]entry
}

// NewRepo builds a catalogue reader. Reads may come from the replica: the
// catalogue is configuration, and a few seconds of replication lag on opening
// hours is harmless.
func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{
		pool:  pool,
		ttl:   DefaultTTL,
		now:   time.Now,
		cache: make(map[uuid.UUID]entry),
	}
}

// WithTTL overrides the cache lifetime. Used by tests.
func (r *Repo) WithTTL(ttl time.Duration) *Repo {
	r.ttl = ttl
	return r
}

// WithClock overrides the clock. Used by tests to expire the cache without
// sleeping.
func (r *Repo) WithClock(now func() time.Time) *Repo {
	r.now = now
	return r
}

// Get returns a facility, from cache when it is fresh.
//
// A cache hit issues no query at all, which is what lets the cheap validation
// rejections in §4.2 happen without touching the database.
func (r *Repo) Get(ctx context.Context, id uuid.UUID) (*Facility, error) {
	if f, ok := r.cached(id); ok {
		return f, nil
	}

	f, err := r.load(ctx, id)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[id] = entry{facility: *f, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()

	return f, nil
}

func (r *Repo) cached(id uuid.UUID) (*Facility, bool) {
	r.mu.RLock()
	e, ok := r.cache[id]
	r.mu.RUnlock()

	if !ok || r.now().After(e.expires) {
		return nil, false
	}
	f := e.facility
	return &f, true
}

// Invalidate drops a facility from the cache. Called on the Redis pub/sub
// invalidation a manager's edit publishes, so a closure takes effect across all
// replicas without waiting out the TTL.
func (r *Repo) Invalidate(id uuid.UUID) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// InvalidateAll drops the whole cache.
func (r *Repo) InvalidateAll() {
	r.mu.Lock()
	r.cache = make(map[uuid.UUID]entry)
	r.mu.Unlock()
}

// List returns the active catalogue.
//
// Not cached: it is a handful of rows behind an HTTP cache header, and caching a
// list separately from the per-id entries would create two copies of the
// catalogue that could disagree about the same facility.
func (r *Repo) List(ctx context.Context) ([]Facility, error) {
	rows, err := r.pool.Query(ctx, queries.Get(queries.FacilityList))
	if err != nil {
		return nil, fmt.Errorf("facility: list: %w", err)
	}
	defer rows.Close()

	var out []Facility
	for rows.Next() {
		f, err := scanFacility(rows)
		if err != nil {
			return nil, fmt.Errorf("facility: list: %w", err)
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("facility: list: %w", err)
	}
	return out, nil
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanFacility(s scanner) (*Facility, error) {
	var (
		f                                     Facility
		opensAt, closesAt                     pgtype.Time
		granularity, minDuration, maxDuration pgtype.Interval
	)
	if err := s.Scan(
		&f.ID, &f.Name, &f.Sport, &f.IsExclusive, &f.Capacity,
		&opensAt, &closesAt, &granularity, &minDuration, &maxDuration, &f.IsActive,
	); err != nil {
		return nil, err
	}

	f.OpensAt = time.Duration(opensAt.Microseconds) * time.Microsecond
	f.ClosesAt = time.Duration(closesAt.Microseconds) * time.Microsecond
	f.Granularity = intervalToDuration(granularity)
	f.MinDuration = intervalToDuration(minDuration)
	f.MaxDuration = intervalToDuration(maxDuration)
	return &f, nil
}

func (r *Repo) load(ctx context.Context, id uuid.UUID) (*Facility, error) {
	f, err := scanFacility(r.pool.QueryRow(ctx, queries.Get(queries.FacilityGet), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("facility: load %s: %w", id, err)
	}
	return f, nil
}

// intervalToDuration flattens a Postgres interval.
//
// Months are treated as 30 days. That is wrong in general and irrelevant here:
// these intervals are booking durations and slot granularities, measured in
// minutes and hours, and a facility with a month-long granularity is not a
// facility.
func intervalToDuration(iv pgtype.Interval) time.Duration {
	return time.Duration(iv.Microseconds)*time.Microsecond +
		time.Duration(iv.Days)*24*time.Hour +
		time.Duration(iv.Months)*30*24*time.Hour
}
