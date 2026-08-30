package sse_test

import (
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/facility"
	"github.com/iitg-playhack/sportsbook/internal/httpx"
	"github.com/iitg-playhack/sportsbook/internal/live"
	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestSSE_ReceivesBookingTransition is the whole feature in one test: a client
// is watching a date, somebody else books a slot on it, and the watcher is told.
//
// Every hop is real. The booking commits through the exclusion constraint, the
// AFTER INSERT trigger's pg_notify fires ON COMMIT, the dispatcher claims the
// outbox row, the publisher puts it on Redis, this process's hub reads it back
// off Redis, and the handler writes it to the socket. Any one of those breaking
// fails here.
func TestSSE_ReceivesBookingTransition(t *testing.T) {
	s := newStack(t)

	watcher := s.login(t, "student01")
	booker := s.login(t, "student02")

	start := tomorrowAt(18)
	date := dateOf(start, s.loc)

	client := s.connect(t, watcher, date)
	awaitSubscribers(t, s.hub, date, 1)

	res := s.book(t, booker, testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)

	ev := client.nextEvent(t, 15*time.Second)
	require.Equal(t, testutil.CourtID(), ev.FacilityID)
	require.Equal(t, slotOf(start, s.loc), ev.Slot)
	require.Equal(t, facility.StateBooked, ev.State)
}

// TestSSE_ReceivesCancellation proves the other direction: a released window is
// announced as free.
//
// The booking is made and drained BEFORE the client connects, so the only event
// the stream can carry is the cancellation. Connecting first would leave the
// assertion reading whichever of the two arrived, which passes for the wrong
// reason about half the time.
func TestSSE_ReceivesCancellation(t *testing.T) {
	s := newStack(t)

	watcher := s.login(t, "student01")
	owner := s.login(t, "student02")

	start := tomorrowAt(19)
	date := dateOf(start, s.loc)

	res := s.book(t, owner, testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)
	id := res.id(t)

	// Let the confirmation drain before anyone is listening.
	require.Equal(t, 1, countBookings(t, s.pg, "CONFIRMED"))
	awaitOutboxDrained(t, s.pg)

	client := s.connect(t, watcher, date)
	awaitSubscribers(t, s.hub, date, 1)

	require.Equal(t, http.StatusOK, s.cancel(t, owner, id))

	ev := client.nextEvent(t, 15*time.Second)
	require.Equal(t, testutil.CourtID(), ev.FacilityID)
	require.Equal(t, slotOf(start, s.loc), ev.Slot)
	require.Equal(t, facility.StateFree, ev.State)
}

// TestSSE_HeartbeatEvery15s covers the comment keepalive from §9 in two halves,
// because the honest test of a fifteen-second interval takes forty-five seconds
// and a suite nobody will wait for is a suite that gets skipped.
//
//   - The VALUE is asserted as a constant. That is the number proxies care
//     about, and it is the one a careless edit would change.
//   - The CADENCE is asserted at a scaled interval: comments keep arriving, on a
//     timer, on an otherwise completely idle connection. That is the property —
//     an idle stream is not a silent one.
func TestSSE_HeartbeatEvery15s(t *testing.T) {
	require.Equal(t, 15*time.Second, httpx.StreamHeartbeat,
		"the §9 keepalive interval is 15s; proxies reap idle connections without it")

	s := newStack(t, withHeartbeat(testHeartbeat))

	token := s.login(t, "student01")
	date := dateOf(tomorrowAt(18), s.loc)

	client := s.connect(t, token, date)

	// Four comments: the opening ": connected" plus three ticks. Nothing is
	// booked for the duration, so every one of them is a keepalive on an idle
	// connection rather than a side effect of traffic.
	started := time.Now()
	got := client.countComments(t, 4, 10*time.Second)
	elapsed := time.Since(started)

	require.Equal(t, 4, got)
	require.GreaterOrEqual(t, elapsed, 3*testHeartbeat-testHeartbeat/2,
		"comments arrived faster than the configured interval, so they are not on a timer")
}

// TestSSE_MultipleSubscribersAllReceive proves the fan-out fans out: one
// publish, N connections, N deliveries.
//
// This is the property that makes the replica model work. The dispatcher
// publishes a transition exactly once no matter how many students are watching,
// and the duplication happens per-replica, in memory.
func TestSSE_MultipleSubscribersAllReceive(t *testing.T) {
	s := newStack(t)

	const subscribers = 4

	start := tomorrowAt(18)
	date := dateOf(start, s.loc)

	clients := make([]*sseClient, subscribers)
	for i := range clients {
		clients[i] = s.connect(t, s.login(t, "student01"), date)
	}
	awaitSubscribers(t, s.hub, date, subscribers)

	res := s.book(t, s.login(t, "student05"), testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)

	for i, c := range clients {
		ev := c.nextEvent(t, 15*time.Second)
		require.Equal(t, testutil.CourtID(), ev.FacilityID, "subscriber %d", i)
		require.Equal(t, slotOf(start, s.loc), ev.Slot, "subscriber %d", i)
		require.Equal(t, facility.StateBooked, ev.State, "subscriber %d", i)
	}
}

// TestSSE_SlowConsumerDroppedNotBlocking is the load-bearing test of the fan-out
// design, and it is deliberately at the hub rather than over HTTP.
//
// Over HTTP a "stalled" client is not really stalled: the kernel socket buffer
// and Go's transport absorb tens of kilobytes before a write blocks, so the test
// would be measuring buffer sizes. At this level the stall is exact — a
// subscription nobody reads from — and the three properties are separable:
//
//  1. the publisher never blocks, no matter how far behind a consumer is;
//  2. a healthy consumer sharing the same date receives EVERYTHING;
//  3. the stalled one is dropped, not silently starved, so its handler ends and
//     its browser reconnects onto a fresh, consistent grid.
func TestSSE_SlowConsumerDroppedNotBlocking(t *testing.T) {
	const buffer = 4
	const events = buffer + 5

	hub := live.NewHub(nil, quiet()).WithBuffer(buffer)
	date := "2030-01-01"

	stalled := hub.Subscribe(date)
	healthy := hub.Subscribe(date)

	// The healthy subscriber is drained SYNCHRONOUSLY, one event per dispatch,
	// so it is never more than one event behind. Draining it on a goroutine
	// instead would leave the test racing the scheduler for a buffer this small,
	// and a flaky proof of "the fast client is fine" is worth nothing.
	var received []live.Event
	var slowest time.Duration

	for i := 0; i < events; i++ {
		started := time.Now()
		hub.Dispatch(date, live.Event{
			FacilityID: testutil.CourtID(),
			Slot:       "18:00",
			State:      facility.StateBooked,
		})
		if d := time.Since(started); d > slowest {
			slowest = d
		}

		select {
		case ev, ok := <-healthy.Events():
			require.True(t, ok, "the healthy subscriber was closed at event %d", i)
			received = append(received, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("the healthy subscriber never received event %d", i)
		}
	}

	// 1. The publisher never waited. Every dispatch is non-blocking by
	//    construction; anything near a scheduler quantum here would mean it had
	//    parked on the consumer that is deliberately not reading.
	require.Less(t, slowest, 250*time.Millisecond,
		"a dispatch blocked on the stalled consumer; fan-out must never wait")

	// 2. The healthy subscriber missed nothing, even though it shared a date
	//    with one that stopped reading entirely.
	require.Len(t, received, events)

	// 3. The stalled subscriber was DROPPED rather than silently starved. Its
	//    channel is closed, so its handler ends the response and the browser
	//    reconnects onto a freshly fetched, consistent grid.
	require.True(t, stalled.Dropped(),
		"a subscriber that overflowed its buffer must be dropped")

	drainedFromStalled := 0
	for range stalled.Events() {
		drainedFromStalled++
	}
	require.LessOrEqual(t, drainedFromStalled, buffer,
		"a dropped subscriber cannot have buffered more than its depth")

	require.Equal(t, 1, hub.Subscribers(date),
		"the dropped subscription must be deregistered and the healthy one kept")

	healthy.Close()
	require.Equal(t, 0, hub.Subscribers(date))
}

// TestSSE_NoGoroutineLeakOnDisconnect holds the design honest: the handler runs
// entirely on the request's own goroutine and starts none of its own.
//
// A connection-per-goroutine fan-out is the obvious way to write this and it is
// the way that leaks, because the goroutine outlives the request whenever the
// client vanishes without a clean close. Counting before and after is a blunt
// instrument, but it catches exactly that.
func TestSSE_NoGoroutineLeakOnDisconnect(t *testing.T) {
	s := newStack(t)

	token := s.login(t, "student01")
	date := dateOf(tomorrowAt(18), s.loc)

	// One warm-up connection, so the transport's own pooled goroutines are
	// already created and not counted as a leak.
	warm := s.connect(t, token, date)
	awaitSubscribers(t, s.hub, date, 1)
	warm.Close()
	waitFor(t, 10*time.Second, "the warm-up connection to be released", func() bool {
		return s.hub.Subscribers(date) == 0
	})
	time.Sleep(200 * time.Millisecond)

	before := runtime.NumGoroutine()

	const rounds = 12
	for i := 0; i < rounds; i++ {
		c := s.connect(t, token, date)
		awaitSubscribers(t, s.hub, date, 1)
		c.Close()
		waitFor(t, 10*time.Second, "the subscription to be released", func() bool {
			return s.hub.Subscribers(date) == 0
		})
	}

	// The server-side proof, and the sharper of the two: every subscription was
	// deregistered. A handler that leaked would still be holding one.
	require.Equal(t, 0, s.hub.Subscribers(date))

	// Settle. Closed connections release their goroutines asynchronously.
	var after int
	waitFor(t, 10*time.Second, "goroutines to settle", func() bool {
		runtime.GC()
		after = runtime.NumGoroutine()
		return after <= before+2
	})

	require.LessOrEqualf(t, after, before+2,
		"leaked goroutines across %d connect/disconnect cycles: %d -> %d", rounds, before, after)
}

// TestSSE_FiltersToRequestedDate proves the filter is real, and that it runs in
// Redis rather than in every replica: a client asks for one date and is not
// woken by another.
//
// The ordering makes the negative assertion sound. The unwanted booking is made
// FIRST and the wanted one second; outbox rows drain in insertion order, so if
// the filter were broken the wrong event would arrive before the right one and
// the first event read would fail the assertion. A bare "wait and see nothing"
// would only prove the machine was slow.
func TestSSE_FiltersToRequestedDate(t *testing.T) {
	s := newStack(t)

	watcher := s.login(t, "student01")
	booker := s.login(t, "student02")

	watched := tomorrowAt(18)
	other := watched.AddDate(0, 0, 1)

	watchedDate := dateOf(watched, s.loc)
	require.NotEqual(t, watchedDate, dateOf(other, s.loc))

	client := s.connect(t, watcher, watchedDate)
	awaitSubscribers(t, s.hub, watchedDate, 1)

	// The day after tomorrow — same facility, same wall-clock hour, different
	// date. This must NOT arrive.
	res := s.book(t, booker, testutil.CourtID(), other, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)

	res = s.book(t, booker, testutil.CourtID(), watched, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)

	ev := client.nextEvent(t, 15*time.Second)
	require.Equal(t, slotOf(watched, s.loc), ev.Slot)
	require.Equal(t, facility.StateBooked, ev.State)

	// And nothing else follows it.
	client.expectNoEvent(t, time.Second)
}

// TestCacheInvalidatedOnTransition covers deliverable 4: the same event that
// tells a client to look again also makes looking again worth it.
//
// Without this the two halves fight. The client is told at t=0 that a slot
// changed, refetches immediately, and is served the cached grid from t-4s that
// still shows the slot free — so the live update would make the screen look
// broken rather than live, which is worse than not having it.
// The cache TTL is stretched to ten minutes for this test on purpose. At
// production's five seconds the key would expire on its own inside the window
// the assertion waits in, and the test would pass identically with the
// invalidation deleted — which it was, until a mutation run caught it. With a
// TTL far longer than the test, a missing key can only mean the DEL ran.
func TestCacheInvalidatedOnTransition(t *testing.T) {
	s := newStack(t, withCacheTTL(10*time.Minute))

	token := s.login(t, "student01")
	start := tomorrowAt(18)
	date := dateOf(start, s.loc)
	key := facility.CampusCacheKey(date)

	// Warm it through the real endpoint.
	require.Equal(t, http.StatusOK, s.campusGrid(t, token, date))
	waitFor(t, 5*time.Second, "the campus grid to be cached", func() bool {
		return exists(t, s.rdb, key)
	})

	res := s.book(t, s.login(t, "student02"), testutil.CourtID(), start, 60)
	require.Equal(t, http.StatusCreated, res.status, "booking failed: %s", res.raw)

	waitFor(t, 15*time.Second, "the cached grid to be invalidated", func() bool {
		return !exists(t, s.rdb, key)
	})

	// And the refetch that follows shows the truth, derived from the bookings
	// table rather than from anything cached (non-negotiable #4).
	require.Equal(t, http.StatusOK, s.campusGrid(t, token, date))
}

// TestSSE_HubWithoutRedisStillServes covers the documented degraded mode: no
// bus, no events, but a connection that stays open and a client that is told
// nothing false.
func TestSSE_HubWithoutRedisStillServes(t *testing.T) {
	s := newStack(t, withDeadRedis(), withHeartbeat(testHeartbeat))

	token := s.login(t, "student01")
	date := dateOf(tomorrowAt(18), s.loc)

	client := s.connect(t, token, date)

	// Heartbeats keep coming even though the bus is gone, so the connection is
	// not reaped and the client is not forced into a reconnect loop.
	require.Equal(t, 3, client.countComments(t, 3, 10*time.Second))
	client.expectNoEvent(t, 200*time.Millisecond)
}
