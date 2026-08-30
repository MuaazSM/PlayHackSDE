package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/live"
)

// StreamHeartbeat is the comment interval from §9.
//
// Fifteen seconds is chosen against infrastructure, not against the product.
// Proxies and load balancers reap idle connections — nginx defaults to 60s,
// most cloud LBs to 60–350s — and an SSE stream is idle by design for most of
// the day. A comment line every fifteen seconds keeps the socket demonstrably
// alive at a cost of about two bytes a minute per client, and gives a client
// three chances to see traffic before the tightest common idle timeout.
const StreamHeartbeat = 15 * time.Second

// heartbeatComment is a bare SSE comment: ignored by EventSource, invisible to
// the application, and enough to keep an intermediary from closing the socket.
const heartbeatComment = ": ping\n\n"

// SSE serves GET /api/v1/stream?date=YYYY-MM-DD (§9).
//
// One-directional by construction, and that is why it is SSE rather than a
// WebSocket: the client has nothing to say on this channel. Everything a student
// does is already a request against the write path, where idempotency, rate
// limiting and shedding apply. A bidirectional socket here would be a second,
// unpoliced way into the system in exchange for a feature nobody needs.
type SSE struct {
	hub       *live.Hub
	loc       *time.Location
	heartbeat time.Duration
}

// NewSSE builds the stream handler over a hub.
func NewSSE(hub *live.Hub, loc *time.Location) *SSE {
	if loc == nil {
		loc = time.UTC
	}
	return &SSE{hub: hub, loc: loc, heartbeat: StreamHeartbeat}
}

// WithHeartbeat overrides the comment interval. Used by tests, which cannot
// afford to wait out the production cadence to observe two of them.
func (s *SSE) WithHeartbeat(d time.Duration) *SSE {
	if d > 0 {
		s.heartbeat = d
	}
	return s
}

// Stream holds the connection open and writes events as they arrive.
//
// The whole handler runs on the request's own goroutine. It starts none, which
// is what makes teardown provable rather than hopeful: when the client goes
// away, r.Context() is cancelled, the select returns, the deferred Close
// deregisters the subscription, and there is nothing else to have leaked.
func (s *SSE) Stream(w http.ResponseWriter, r *http.Request) {
	// Established before anything is written. Without a Flusher every event
	// would sit in the response buffer until the connection closed, which is a
	// stream that delivers everything at once, at the end — worse than polling.
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Unmapped, so the envelope renders it as a 500 — which is right. A
		// response writer that cannot flush is a defect in the middleware chain,
		// not something the caller did.
		Error(w, r, errors.New("stream: response writer does not implement http.Flusher"))
		return
	}

	date, err := streamDate(r, s.loc)
	if err != nil {
		Error(w, r, err)
		return
	}

	// Subscribed BEFORE the headers go out. A client that has been told the
	// stream is open must not be able to miss an event committed in the gap
	// between that promise and the subscription actually existing.
	sub := s.hub.Subscribe(date)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which would hold events back
	// until the buffer filled. This is the documented opt-out.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// An opening comment, so the client learns the stream is live now rather
	// than on the first booking of the day. EventSource fires onopen on the
	// headers; anything reading the body needs a byte.
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client hung up, or the server is shutting down. Nothing to clean
			// up beyond the deferred Close.
			return

		case <-ticker.C:
			if _, err := io.WriteString(w, heartbeatComment); err != nil {
				return
			}
			flusher.Flush()

		case ev, ok := <-sub.Events():
			if !ok {
				// The hub closed this subscription: either it is shutting down,
				// or this connection fell too far behind and was dropped so it
				// could not stall delivery for everybody else. Ending the
				// response is the signal — EventSource reconnects on its own and
				// refetches the grid, which is the correct recovery from both.
				return
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent renders one event in the SSE wire format.
//
// A named event ("slot") rather than the default message type, so a client can
// addEventListener for exactly this and a later phase can add a second event
// kind on the same connection without every listener having to demultiplex.
func writeEvent(w io.Writer, ev live.Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		// Cannot happen for this struct; dropping one event is still better than
		// tearing down a working stream over it.
		return nil
	}
	_, err = fmt.Fprintf(w, "event: slot\ndata: %s\n\n", raw)
	return err
}

// streamDate reads ?date=YYYY-MM-DD, defaulting to today in the campus timezone.
//
// Same rule, and the same reason, as the availability handlers: "today" is
// resolved in IST, because at 05:00 IST the UTC date is still yesterday and a
// student would be subscribed to the wrong day's channel every morning.
//
// Deliberately its own function rather than a call into Handlers.parseDate. The
// stream is not wired through Handlers — it needs a hub and none of the booking
// domain — and widening that constructor to carry a dependency only this route
// uses would couple every availability test to the live-update stack.
func streamDate(r *http.Request, loc *time.Location) (string, error) {
	raw := r.URL.Query().Get("date")
	if raw == "" || raw == "today" {
		return time.Now().In(loc).Format("2006-01-02"), nil
	}
	if _, err := time.ParseInLocation("2006-01-02", raw, loc); err != nil {
		return "", fmt.Errorf("%w: date must be YYYY-MM-DD", ErrBadRequest)
	}
	return raw, nil
}

// bearerFromQuery promotes ?access_token= into an Authorization header.
//
// It exists for exactly one reason: the browser EventSource API cannot set
// request headers, so the client §9 names has no way to present a bearer token
// the way every other route expects one. Rather than leaving the stream
// unauthenticated, this accepts the token in the query string for this route
// only.
//
// THE TRADE IS REAL AND IT IS SCOPED ON PURPOSE. A token in a URL can end up in
// access logs and in a Referer header, which a token in a header does not. It is
// applied to /stream alone, it never overrides a header that was actually sent,
// and the stream carries only what the public availability grid already shows.
// If a fetch-based EventSource polyfill lands on the frontend, this should go.
func bearerFromQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if tok := r.URL.Query().Get("access_token"); tok != "" {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		next.ServeHTTP(w, r)
	})
}
