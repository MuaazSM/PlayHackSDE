package facility

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Slot states, as rendered on the grid.
const (
	StateFree    = "free"
	StateHeld    = "held"
	StateBooked  = "booked"
	StateClosed  = "closed"
	StateFilling = "filling"
	StateFull    = "full"
)

// Slot is one cell of a facility's day.
type Slot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	State string    `json:"state"`

	// Remaining and Capacity are set for shared facilities only. A nil pointer
	// means the question does not apply, which is different from zero left.
	Remaining *int `json:"remaining,omitempty"`
	Capacity  *int `json:"capacity,omitempty"`
}

// DayAvailability is one facility's day.
type DayAvailability struct {
	FacilityID uuid.UUID `json:"facility_id"`
	Date       string    `json:"date"`
	Slots      []Slot    `json:"slots"`
}

// GridFacility is a row header of the campus grid.
type GridFacility struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Sport       string    `json:"sport"`
	IsExclusive bool      `json:"is_exclusive"`
}

// GridSlot is a column header.
type GridSlot struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// CampusGrid is the discovery screen's whole payload.
//
// Grid is a dense array of states indexed [facility][slot], not an object per
// cell. It is rendered as a table, and the dense form keeps the payload small
// enough that the screen is one request rather than one per facility.
type CampusGrid struct {
	Date       string         `json:"date"`
	Facilities []GridFacility `json:"facilities"`
	Slots      []GridSlot     `json:"slots"`
	Grid       [][]string     `json:"grid"`
}

// Availability derives what is bookable, at read time, from the bookings table.
//
// There is no materialised availability table and no is_available column
// (non-negotiable #4). Everything here is computed per request from the same
// rows the write path inserts into, which is why derived availability cannot
// drift from reality — there is nothing separate to drift.
type Availability struct {
	replica *pgxpool.Pool
	rdb     *redis.Client
	tz      string
	ttl     time.Duration
	log     *slog.Logger
}

// CacheTTL is how long a campus grid is served from Redis.
//
// FIVE SECONDS OF STALENESS IS SAFE BY CONSTRUCTION, and this is the whole
// argument for caching a page about occupancy at all:
//
// A stale "free" costs the user one wasted tap and a fast 409 — the insert is
// still attempted, and the exclusion constraint still decides. A stale "booked"
// costs one slot hidden for up to five seconds. NEITHER CAN PRODUCE A WRONG
// BOOKING, because nothing on the write path ever reads this cache; the write
// path does not read availability at all.
//
// That is why Redis may hold this and may not hold anything the booking depends
// on (non-negotiable #3). Wipe Redis mid-demo and the only effect is that the
// next grid request is a few milliseconds slower.
const CacheTTL = 5 * time.Second

// NewAvailability builds the read path.
//
// It takes the REPLICA pool: availability is the highest-volume read in the
// system (reads beat writes about 100:1) and keeping it off the primary leaves
// the primary's connections for the write path. Replication lag is bounded by
// the same argument as the cache above.
func NewAvailability(replica *pgxpool.Pool, rdb *redis.Client, tz string, log *slog.Logger) *Availability {
	if log == nil {
		log = slog.Default()
	}
	return &Availability{replica: replica, rdb: rdb, tz: tz, ttl: CacheTTL, log: log}
}

// WithTTL overrides the cache lifetime. Used by tests.
func (a *Availability) WithTTL(ttl time.Duration) *Availability {
	a.ttl = ttl
	return a
}

// ForFacility returns one facility's day.
//
// Not cached: it is a single facility's grid, cheap, and the confirm screen is
// the one place a student most wants the truth rather than a five-second-old
// version of it.
func (a *Availability) ForFacility(ctx context.Context, f *Facility, date string) (*DayAvailability, error) {
	query := queries.AvailabilityFacility
	if !f.IsExclusive {
		query = queries.AvailabilityShared
	}

	rows, err := a.replica.Query(ctx, queries.Get(query), f.ID, date, a.tz)
	if err != nil {
		return nil, fmt.Errorf("availability: facility %s: %w", f.ID, err)
	}
	defer rows.Close()

	out := &DayAvailability{FacilityID: f.ID, Date: date, Slots: []Slot{}}
	for rows.Next() {
		var s Slot
		if f.IsExclusive {
			if err := rows.Scan(&s.Start, &s.End, &s.State); err != nil {
				return nil, fmt.Errorf("availability: scan: %w", err)
			}
		} else {
			var remaining, capacity int
			if err := rows.Scan(&s.Start, &s.End, &s.State, &remaining, &capacity); err != nil {
				return nil, fmt.Errorf("availability: scan: %w", err)
			}
			s.Remaining, s.Capacity = &remaining, &capacity
		}
		out.Slots = append(out.Slots, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("availability: facility %s: %w", f.ID, err)
	}
	return out, nil
}

// Campus returns the whole discovery grid for a date.
//
// Redis first, Postgres always as the fallback. Serving this from Redis WITHOUT
// a Postgres fallback would make a cache outage a feature outage; here it is a
// latency change nobody notices.
func (a *Availability) Campus(ctx context.Context, date string) (*CampusGrid, error) {
	key := campusKey(date)

	if grid, ok := a.fromCache(ctx, key); ok {
		return grid, nil
	}

	grid, err := a.campusFromDB(ctx, date)
	if err != nil {
		return nil, err
	}

	a.toCache(ctx, key, grid)
	return grid, nil
}

// CampusCached returns the grid ONLY if it is already warm in Redis, and never
// falls through to Postgres.
//
// Campus is the right call for a page render: a miss should cost a query, not an
// empty screen. This one exists for the 409 alternatives path (§5.3), which is
// the opposite trade — it is spending someone's rejection latency, it has a 40 ms
// budget for the whole enrichment, and an alternative is a nicety. So a cold
// cache here means "fall back to the two narrow SQL queries", and the caller,
// not this method, decides that.
//
// Returning a miss rather than querying is the whole point; do not "fix" this by
// delegating to Campus.
func (a *Availability) CampusCached(ctx context.Context, date string) (*CampusGrid, bool) {
	return a.fromCache(ctx, campusKey(date))
}

// campusKey is the one place the cache key is spelled. Two callers reading the
// same entry under two different keys would look exactly like a cache that never
// warms up.
func campusKey(date string) string { return "avail:" + date }

func (a *Availability) fromCache(ctx context.Context, key string) (*CampusGrid, bool) {
	if a.rdb == nil {
		return nil, false
	}

	raw, err := a.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false
	}
	if err != nil {
		// Redis being unreachable is not an error the caller should see. Fall
		// through to Postgres and serve the truth, slightly slower.
		a.log.WarnContext(ctx, "availability cache unavailable, falling through to postgres",
			"err", err, "key", key)
		return nil, false
	}

	var grid CampusGrid
	if err := json.Unmarshal(raw, &grid); err != nil {
		a.log.WarnContext(ctx, "availability cache entry unreadable, ignoring", "err", err, "key", key)
		return nil, false
	}
	return &grid, true
}

func (a *Availability) toCache(ctx context.Context, key string, grid *CampusGrid) {
	if a.rdb == nil {
		return
	}
	raw, err := json.Marshal(grid)
	if err != nil {
		return
	}
	// Best effort. A failed write costs one cache miss, never a wrong answer.
	if err := a.rdb.Set(ctx, key, raw, a.ttl).Err(); err != nil {
		a.log.WarnContext(ctx, "availability cache write failed", "err", err, "key", key)
	}
}

// campusFromDB runs the single campus query and pivots it into the dense shape.
func (a *Availability) campusFromDB(ctx context.Context, date string) (*CampusGrid, error) {
	rows, err := a.replica.Query(ctx, queries.Get(queries.AvailabilityCampus), date, a.tz)
	if err != nil {
		return nil, fmt.Errorf("availability: campus: %w", err)
	}
	defer rows.Close()

	grid := &CampusGrid{
		Date:       date,
		Facilities: []GridFacility{},
		Slots:      []GridSlot{},
		Grid:       [][]string{},
	}

	facilityIndex := map[uuid.UUID]int{}
	slotIndex := map[time.Time]int{}

	for rows.Next() {
		var (
			f     GridFacility
			slot  GridSlot
			state string
		)
		if err := rows.Scan(&f.ID, &f.Name, &f.Sport, &f.IsExclusive,
			&slot.Start, &slot.End, &state); err != nil {
			return nil, fmt.Errorf("availability: campus: scan: %w", err)
		}

		fi, ok := facilityIndex[f.ID]
		if !ok {
			fi = len(grid.Facilities)
			facilityIndex[f.ID] = fi
			grid.Facilities = append(grid.Facilities, f)
			grid.Grid = append(grid.Grid, nil)
		}

		si, ok := slotIndex[slot.Start]
		if !ok {
			si = len(grid.Slots)
			slotIndex[slot.Start] = si
			grid.Slots = append(grid.Slots, slot)
		}

		for len(grid.Grid[fi]) <= si {
			grid.Grid[fi] = append(grid.Grid[fi], "")
		}
		grid.Grid[fi][si] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("availability: campus: %w", err)
	}

	// Every row is a rectangle of the same width, so the client can index
	// grid[facility][slot] without bounds checks per row.
	for i := range grid.Grid {
		for len(grid.Grid[i]) < len(grid.Slots) {
			grid.Grid[i] = append(grid.Grid[i], StateClosed)
		}
	}

	return grid, nil
}
