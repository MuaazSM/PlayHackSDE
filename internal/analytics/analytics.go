// Package analytics is the manager's read-only view of the system: what got
// used, when people wanted it, who did not turn up, and whether the waitlist
// actually recovered the slots cancellations freed.
//
// EVERYTHING HERE IS DERIVED BY QUERY. There is no rollup table, no materialised
// view, and nothing in this package runs on the write path — the booking
// transaction does not know analytics exists. That is not laziness about
// performance; it is the same argument as non-negotiable #4. A counter
// maintained on write is a second statement of a fact the bookings table
// already holds, and two statements of one fact eventually disagree. The one
// that disagrees quietly is the one a manager builds a timetable on.
//
// The scale makes the choice free: a few hundred bookings a day means the
// honest query reads a few thousand rows over a month. If this were a million
// bookings a day the answer would be a nightly rollup with a stated staleness,
// not a counter smuggled into the booking transaction.
package analytics

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

// CacheTTL is how long a report is served from Redis.
//
// Sixty seconds rather than the availability path's five. Availability is
// staleness a student ACTS on — they tap a slot that a stale grid called free —
// so it is kept tight. A month's utilisation chart moving by one booking is not
// a decision anybody makes differently, and a manager refreshing the console
// should not be able to put five aggregate queries per second on the replica.
//
// As everywhere else, Redis here is a cache and nothing more (non-negotiable
// #3). Wipe it and the next report costs five queries instead of one GET.
const CacheTTL = 60 * time.Second

// MaxRangeDays bounds the requested window.
//
// Not a performance guard — it is a footgun guard. The utilisation query
// materialises one cell per facility per open hour per day, so a range of
// "1970-01-01 to 2100-01-01" is a request to build tens of millions of rows on
// the read replica the student-facing availability path shares. A year is more
// history than this system will ever have.
const MaxRangeDays = 366

// HoursPerDay is the heatmap's width. Spelled once so the dense matrix, the
// bounds check and the tests cannot disagree about it.
const HoursPerDay = 24

// DaysPerWeek is the heatmap's height, indexed 0 = Monday .. 6 = Sunday to
// match Postgres isodow (1..7) minus one.
const DaysPerWeek = 7

// ErrBadRange means from/to did not parse, or to is before from, or the window
// is longer than MaxRangeDays.
var ErrBadRange = errors.New("invalid date range")

// FacilityHour is one cell of the utilisation chart.
//
// BookedHours and AvailableHours are reported alongside the ratio on purpose. A
// bare 100% hides whether it came from one booking on one day or from thirty
// over a month, and the manager's next question is always which.
type FacilityHour struct {
	FacilityID     uuid.UUID `json:"facility_id"`
	FacilityName   string    `json:"facility_name"`
	Hour           int       `json:"hour"`
	BookedHours    float64   `json:"booked_hours"`
	AvailableHours float64   `json:"available_hours"`
	Utilisation    float64   `json:"utilisation"`
}

// Heatmap is peak demand as a dense 7x24 matrix, indexed [weekday][hour] with
// weekday 0 = Monday.
//
// Dense rather than a list of non-empty cells, for the same reason the campus
// grid is dense: it is rendered as a table, and a client that has to decide
// what an absent cell means will eventually decide wrongly. An hour nobody
// wanted is a zero, which is a fact, not a gap.
type Heatmap struct {
	Weekdays []string `json:"weekdays"`
	Hours    []int    `json:"hours"`
	Cells    [][]int  `json:"cells"`
	Peak     PeakCell `json:"peak"`
}

// PeakCell names the busiest cell. Count zero means nothing was requested in
// the window at all, and Weekday/Hour are then meaningless.
type PeakCell struct {
	Weekday int `json:"weekday"`
	Hour    int `json:"hour"`
	Count   int `json:"count"`
}

// NoShowRate is one facility's attendance record. Feeds M-6.
type NoShowRate struct {
	FacilityID   uuid.UUID `json:"facility_id"`
	FacilityName string    `json:"facility_name"`
	Total        int       `json:"total"`
	NoShows      int       `json:"no_shows"`
	Rate         float64   `json:"rate"`
}

// UnmetDemand is waitlist depth for one facility at one hour of the day.
type UnmetDemand struct {
	FacilityID   uuid.UUID `json:"facility_id"`
	FacilityName string    `json:"facility_name"`
	Hour         int       `json:"hour"`
	Entries      int       `json:"entries"`
}

// SlotRecovery is the waitlist's payoff. Feeds M-7.
type SlotRecovery struct {
	Promoted  int     `json:"promoted"`
	Recovered int     `json:"recovered"`
	Rate      float64 `json:"rate"`
}

// Report is the whole console payload, in one request.
//
// One endpoint rather than five, because every panel on the manager screen is
// scoped to the same date range and five round trips would let them disagree
// about the window — a utilisation chart and a no-show table describing
// different fortnights is worse than either being slightly stale.
type Report struct {
	From         string         `json:"from"`
	To           string         `json:"to"`
	Utilisation  []FacilityHour `json:"utilisation"`
	PeakDemand   Heatmap        `json:"peak_demand"`
	NoShow       []NoShowRate   `json:"no_show"`
	UnmetDemand  []UnmetDemand  `json:"unmet_demand"`
	SlotRecovery SlotRecovery   `json:"slot_recovery"`
}

// Service answers manager questions off the read replica.
type Service struct {
	replica *pgxpool.Pool
	rdb     *redis.Client
	tz      string
	ttl     time.Duration
	log     *slog.Logger
}

// NewService builds the reporting reader.
//
// It takes the REPLICA pool, and that matters more here than it does for
// availability. These are the only aggregate scans in the system: a manager
// opening the console must not be able to make the 6 PM booking rush queue
// behind a month-wide GROUP BY on the primary. Replication lag on a report
// about last week is not a concept anybody can perceive.
func NewService(replica *pgxpool.Pool, rdb *redis.Client, tz string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if tz == "" {
		tz = "UTC"
	}
	return &Service{replica: replica, rdb: rdb, tz: tz, ttl: CacheTTL, log: log}
}

// WithTTL overrides the cache lifetime. Used by tests.
func (s *Service) WithTTL(ttl time.Duration) *Service {
	s.ttl = ttl
	return s
}

// CacheKey is the one place the report's Redis key is spelled.
func CacheKey(from, to string) string { return "analytics:" + from + ":" + to }

// ParseRange validates an inclusive [from, to] pair of local dates.
//
// Both are LOCAL dates, deliberately. A manager asking for "the 14th" means the
// campus day, and resolving that to UTC boundaries is this layer's job — the
// SQL takes the timezone as a parameter for exactly that reason and never
// hardcodes it.
func ParseRange(from, to string) (string, string, error) {
	const layout = "2006-01-02"

	f, err := time.Parse(layout, from)
	if err != nil {
		return "", "", fmt.Errorf("%w: from must be YYYY-MM-DD", ErrBadRange)
	}
	t, err := time.Parse(layout, to)
	if err != nil {
		return "", "", fmt.Errorf("%w: to must be YYYY-MM-DD", ErrBadRange)
	}
	if t.Before(f) {
		return "", "", fmt.Errorf("%w: to is before from", ErrBadRange)
	}
	if days := int(t.Sub(f).Hours()/24) + 1; days > MaxRangeDays {
		return "", "", fmt.Errorf("%w: at most %d days", ErrBadRange, MaxRangeDays)
	}
	return f.Format(layout), t.Format(layout), nil
}

// Report builds the whole console payload for an inclusive local date range.
//
// Redis first, Postgres always as the fallback — a cache outage must be a
// latency change, never a blank console.
func (s *Service) Report(ctx context.Context, from, to string) (*Report, error) {
	from, to, err := ParseRange(from, to)
	if err != nil {
		return nil, err
	}

	key := CacheKey(from, to)
	if rep, ok := s.fromCache(ctx, key); ok {
		return rep, nil
	}

	rep := &Report{
		From:        from,
		To:          to,
		Utilisation: []FacilityHour{},
		NoShow:      []NoShowRate{},
		UnmetDemand: []UnmetDemand{},
	}

	if rep.Utilisation, err = s.utilisation(ctx, from, to); err != nil {
		return nil, err
	}
	if rep.PeakDemand, err = s.heatmap(ctx, from, to); err != nil {
		return nil, err
	}
	if rep.NoShow, err = s.noShow(ctx, from, to); err != nil {
		return nil, err
	}
	if rep.UnmetDemand, err = s.unmetDemand(ctx, from, to); err != nil {
		return nil, err
	}
	if rep.SlotRecovery, err = s.slotRecovery(ctx, from, to); err != nil {
		return nil, err
	}

	s.toCache(ctx, key, rep)
	return rep, nil
}

func (s *Service) utilisation(ctx context.Context, from, to string) ([]FacilityHour, error) {
	rows, err := s.replica.Query(ctx, queries.Get(queries.AnalyticsUtilisation), from, to, s.tz)
	if err != nil {
		return nil, fmt.Errorf("analytics: utilisation: %w", err)
	}
	defer rows.Close()

	out := []FacilityHour{}
	for rows.Next() {
		var c FacilityHour
		if err := rows.Scan(&c.FacilityID, &c.FacilityName, &c.Hour,
			&c.AvailableHours, &c.BookedHours, &c.Utilisation); err != nil {
			return nil, fmt.Errorf("analytics: utilisation: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: utilisation: %w", err)
	}
	return out, nil
}

// heatmap pivots the sparse SQL result into the dense 7x24 matrix.
//
// The pivot lives here rather than in SQL because generating 168 rows to
// describe an empty week is work the database should not do; building the
// zeroed matrix in Go and filling what came back is the same answer for free.
func (s *Service) heatmap(ctx context.Context, from, to string) (Heatmap, error) {
	h := Heatmap{
		Weekdays: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
		Hours:    make([]int, HoursPerDay),
		Cells:    make([][]int, DaysPerWeek),
	}
	for i := range h.Hours {
		h.Hours[i] = i
	}
	for d := range h.Cells {
		h.Cells[d] = make([]int, HoursPerDay)
	}

	rows, err := s.replica.Query(ctx, queries.Get(queries.AnalyticsHeatmap), from, to, s.tz)
	if err != nil {
		return h, fmt.Errorf("analytics: heatmap: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var isodow, hour, count int
		if err := rows.Scan(&isodow, &hour, &count); err != nil {
			return h, fmt.Errorf("analytics: heatmap: scan: %w", err)
		}
		// isodow is 1..7 from Postgres; the matrix is 0..6. Guard anyway — a
		// silently out-of-range index here would panic in a handler.
		d := isodow - 1
		if d < 0 || d >= DaysPerWeek || hour < 0 || hour >= HoursPerDay {
			continue
		}
		h.Cells[d][hour] = count
		if count > h.Peak.Count {
			h.Peak = PeakCell{Weekday: d, Hour: hour, Count: count}
		}
	}
	if err := rows.Err(); err != nil {
		return h, fmt.Errorf("analytics: heatmap: %w", err)
	}
	return h, nil
}

func (s *Service) noShow(ctx context.Context, from, to string) ([]NoShowRate, error) {
	rows, err := s.replica.Query(ctx, queries.Get(queries.AnalyticsNoShow), from, to, s.tz)
	if err != nil {
		return nil, fmt.Errorf("analytics: no-show: %w", err)
	}
	defer rows.Close()

	out := []NoShowRate{}
	for rows.Next() {
		var n NoShowRate
		if err := rows.Scan(&n.FacilityID, &n.FacilityName, &n.Total, &n.NoShows, &n.Rate); err != nil {
			return nil, fmt.Errorf("analytics: no-show: scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: no-show: %w", err)
	}
	return out, nil
}

func (s *Service) unmetDemand(ctx context.Context, from, to string) ([]UnmetDemand, error) {
	rows, err := s.replica.Query(ctx, queries.Get(queries.AnalyticsUnmetDemand), from, to, s.tz)
	if err != nil {
		return nil, fmt.Errorf("analytics: unmet demand: %w", err)
	}
	defer rows.Close()

	out := []UnmetDemand{}
	for rows.Next() {
		var u UnmetDemand
		if err := rows.Scan(&u.FacilityID, &u.FacilityName, &u.Hour, &u.Entries); err != nil {
			return nil, fmt.Errorf("analytics: unmet demand: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: unmet demand: %w", err)
	}
	return out, nil
}

func (s *Service) slotRecovery(ctx context.Context, from, to string) (SlotRecovery, error) {
	var r SlotRecovery
	err := s.replica.QueryRow(ctx, queries.Get(queries.AnalyticsRecovery), from, to, s.tz).
		Scan(&r.Promoted, &r.Recovered, &r.Rate)
	if err != nil {
		return r, fmt.Errorf("analytics: slot recovery: %w", err)
	}
	return r, nil
}

func (s *Service) fromCache(ctx context.Context, key string) (*Report, bool) {
	if s.rdb == nil {
		return nil, false
	}
	raw, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false
	}
	if err != nil {
		s.log.WarnContext(ctx, "analytics cache unavailable, falling through to postgres",
			"err", err, "key", key)
		return nil, false
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		s.log.WarnContext(ctx, "analytics cache entry unreadable, ignoring", "err", err, "key", key)
		return nil, false
	}
	return &rep, true
}

func (s *Service) toCache(ctx context.Context, key string, rep *Report) {
	if s.rdb == nil {
		return
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return
	}
	// Best effort. A failed write costs one cache miss, never a wrong answer.
	if err := s.rdb.Set(ctx, key, raw, s.ttl).Err(); err != nil {
		s.log.WarnContext(ctx, "analytics cache write failed", "err", err, "key", key)
	}
}
