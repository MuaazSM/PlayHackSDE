// Package observability holds the eight Prometheus metrics of
// IMPLEMENTATION.md §14, and the slog handler that stamps request_id on every
// line.
//
// Eight, and only eight. The set is chosen to make CONTENTION visible, not to
// look impressive: every one of them answers a question somebody will actually
// ask while the 6 PM surge is on the screen. A metric nobody looks at during the
// demo is a metric that costs cardinality and buys nothing, so this file is a
// closed list rather than a starting point.
//
//	booking_write_duration_seconds{outcome}   M-2 / M-3, split confirmed|conflict|shed
//	booking_conflicts_total{facility}         where the contention actually is
//	booking_shed_total                        how often the queue bound bit
//	write_queue_depth                         headroom, right now
//	availability_query_duration_seconds       M-4, the read path
//	waitlist_promotions_total{result}         M-7, the second concurrency proof
//	outbox_pending                            dispatcher health
//	replica_lag_seconds                       staleness of the read path
package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Write outcomes. These are the only values booking_write_duration_seconds ever
// carries — a bounded label set, pre-registered below so the panels are not
// blank until the first request of each kind arrives.
const (
	OutcomeConfirmed = "confirmed"
	OutcomeConflict  = "conflict"
	OutcomeShed      = "shed"
)

// Promotion results, for waitlist_promotions_total.
const (
	// PromotionPromoted: a waiting student now holds the freed court.
	PromotionPromoted = "promoted"
	// PromotionEmpty: nobody was waiting. The common case, and not a failure.
	PromotionEmpty = "empty"
	// PromotionLost: another cancel's promotion took the window first. The
	// exclusion constraint said so, which is the only authority that could.
	PromotionLost = "lost"
	// PromotionError: the promotion could not be attempted.
	PromotionError = "error"
)

// writeBuckets straddle the two targets that matter — rejections under 150 ms,
// confirmations under 250 ms. Prometheus' default buckets put both in the same
// bin and would hide exactly the split this histogram exists to show.
var writeBuckets = []float64{.005, .01, .025, .05, .1, .15, .2, .25, .4, .8, 2}

// readBuckets are an order of magnitude tighter: availability is a cached read
// on a 40 ms budget, and default buckets would report every one of them as
// "under 5 ms" or "under 500 ms" with nothing in between.
var readBuckets = []float64{.001, .0025, .005, .01, .025, .04, .075, .15, .5}

var (
	// BookingWriteDuration is the end-to-end latency of POST /api/v1/bookings,
	// measured at the HTTP edge because that is where the p99 targets are
	// written. Timing only the service call would exclude admission control,
	// which is the part that makes losing fast.
	BookingWriteDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "booking_write_duration_seconds",
		Help:    "Booking write latency, split by outcome (confirmed|conflict|shed).",
		Buckets: writeBuckets,
	}, []string{"outcome"})

	// BookingConflicts counts lost races per facility. This is the contention
	// heat map: one facility carrying every conflict is the 6 PM tennis court,
	// and that is worth seeing without reading the database.
	//
	// Labelled by facility NAME, not id: there are seven of them, the
	// cardinality is a constant, and a UUID on a dashboard axis is unreadable.
	BookingConflicts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "booking_conflicts_total",
		Help: "Booking attempts rejected by the exclusion constraint or a full capacity counter.",
	}, []string{"facility"})

	// BookingShed counts requests refused by the write queue bound. Rising
	// together with a flat write_queue_depth means the bound is too low; rising
	// together with a climbing conflict p99 means it is too high.
	BookingShed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "booking_shed_total",
		Help: "Booking writes rejected immediately because the write queue was full.",
	})

	// WriteQueueDepth is writes IN FLIGHT, not the configured bound. The bound
	// is a constant and a constant makes a useless graph; the occupancy against
	// it is the headroom.
	WriteQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "write_queue_depth",
		Help: "Booking writes currently admitted by the shedder.",
	})

	// AvailabilityQueryDuration is the read path, M-4.
	AvailabilityQueryDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "availability_query_duration_seconds",
		Help:    "Availability read latency, cache hits included.",
		Buckets: readBuckets,
	})

	// WaitlistPromotions is the second concurrency proof in counter form:
	// concurrent cancels should produce N promoted and no duplicates.
	WaitlistPromotions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waitlist_promotions_total",
		Help: "Waitlist promotion attempts by result (promoted|empty|lost|error).",
	}, []string{"result"})

	// OutboxPending is dispatcher health. A number that climbs and does not come
	// back down is a dispatcher that has stopped draining — which does not break
	// a booking, but does mean nobody is being told about one.
	OutboxPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_pending",
		Help: "Side-effect rows waiting to be dispatched.",
	})

	// ReplicaLag is how stale the read path is. Zero when DB_REPLICA_URL is
	// unset and availability is served by the primary — not "unknown", because
	// in that configuration the read path genuinely cannot be stale.
	ReplicaLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "replica_lag_seconds",
		Help: "Seconds the availability replica trails the primary. 0 when reads fall back to the primary.",
	})
)

func init() {
	// Pre-register the bounded label sets. A Prometheus vector with no children
	// is omitted from /metrics entirely, so without this the surge dashboard
	// shows "No data" on its most important panel until the first request of
	// each kind lands — which, during a demo, reads as "the metric is broken".
	//
	// facility is deliberately NOT pre-registered: the label set is the
	// catalogue, it is not known here, and a zero-valued placeholder series
	// would be a lie about a facility that has never been contended.
	for _, outcome := range []string{OutcomeConfirmed, OutcomeConflict, OutcomeShed} {
		BookingWriteDuration.WithLabelValues(outcome)
	}
	for _, result := range []string{PromotionPromoted, PromotionEmpty, PromotionLost, PromotionError} {
		WaitlistPromotions.WithLabelValues(result)
	}
}

// ObserveWrite records one booking write. outcome must be one of the Outcome
// constants; anything else is dropped rather than admitted as a new label value,
// because an unbounded outcome label is how a histogram becomes a memory leak.
func ObserveWrite(outcome string, d time.Duration) {
	switch outcome {
	case OutcomeConfirmed, OutcomeConflict, OutcomeShed:
		BookingWriteDuration.WithLabelValues(outcome).Observe(d.Seconds())
	}
}

// RecordConflict counts a lost race against a named facility.
func RecordConflict(facility string) {
	if facility == "" {
		facility = "unknown"
	}
	BookingConflicts.WithLabelValues(facility).Inc()
}

// RecordShed counts one refused write.
func RecordShed() { BookingShed.Inc() }

// SetWriteQueueDepth publishes the shedder's current occupancy.
func SetWriteQueueDepth(n int) { WriteQueueDepth.Set(float64(n)) }

// ObserveAvailabilityQuery records one availability read.
func ObserveAvailabilityQuery(d time.Duration) {
	AvailabilityQueryDuration.Observe(d.Seconds())
}

// ObserveSince records an availability read that started at t. Written for
// `defer observability.ObserveSince(time.Now())`, which is the only form that
// covers every return path in a function with a cache hit, a database fallback
// and three error exits.
func ObserveSince(t time.Time) { ObserveAvailabilityQuery(time.Since(t)) }

// RecordPromotion counts one promotion attempt by result.
func RecordPromotion(result string) {
	switch result {
	case PromotionPromoted, PromotionEmpty, PromotionLost, PromotionError:
		WaitlistPromotions.WithLabelValues(result).Inc()
	}
}

// SetOutboxPending publishes the dispatcher backlog.
func SetOutboxPending(n int64) { OutboxPending.Set(float64(n)) }

// SetReplicaLag publishes read-path staleness in seconds.
func SetReplicaLag(seconds float64) { ReplicaLag.Set(seconds) }

// ---------------------------------------------------------------------------
// Replica lag sampling
// ---------------------------------------------------------------------------

// ReplicaLagInterval is how often the sampler asks. Slow on purpose: this is a
// staleness indicator, and polling it hard would spend replica connections on
// watching the replica.
const ReplicaLagInterval = 5 * time.Second

// SampleReplicaLag polls the replica's replay lag until ctx is done.
//
// Called with dedicated=false — DB_REPLICA_URL unset, availability served by the
// primary — it publishes a flat zero once and returns. That is the honest value:
// there is no second server to trail, so the read path cannot be stale, and a
// gauge that simply stopped reporting would be indistinguishable from a sampler
// that had crashed.
//
// A failing sample is logged at debug and leaves the last value in place. This
// is a dashboard input, not a health check; /readyz already owns reachability.
func SampleReplicaLag(ctx context.Context, pool *pgxpool.Pool, dedicated bool, interval time.Duration, log *slog.Logger) {
	if !dedicated || pool == nil {
		SetReplicaLag(0)
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = ReplicaLagInterval
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		sampleReplicaLagOnce(ctx, pool, log)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func sampleReplicaLagOnce(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var seconds float64
	if err := pool.QueryRow(queryCtx, queries.Get(queries.ReplicaLag)).Scan(&seconds); err != nil {
		log.DebugContext(ctx, "replica lag sample failed", "err", err)
		return
	}
	SetReplicaLag(seconds)
}
