package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/iitg-playhack/sportsbook/internal/analytics"
)

// RoleSecretary sits beside RoleManager: the sports secretary is a student
// office-holder who reads the same reports but does not close facilities.
const RoleSecretary = "SECRETARY"

// RequireAnyRole admits a caller holding ANY of the listed roles.
//
// Separate from RequireRole rather than a rewrite of it. RequireRole guards
// closures and the venue check-in token — write-shaped, manager-only capability
// — and widening it in place would quietly widen those too. Analytics is the
// first read that two roles legitimately share.
func RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				Error(w, r, ErrUnauthenticated)
				return
			}
			if !allowed[p.Role] {
				Error(w, r, fmt.Errorf("%w: %s only",
					ErrForbiddenRole, strings.Join(roles, " or ")))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AnalyticsHandlers is the manager console's one read endpoint (§10.2, FR-17).
type AnalyticsHandlers struct {
	svc *analytics.Service
	loc *time.Location
}

// NewAnalyticsHandlers wires the HTTP edge to the reporting domain.
func NewAnalyticsHandlers(svc *analytics.Service, loc *time.Location) *AnalyticsHandlers {
	if loc == nil {
		loc = time.UTC
	}
	return &AnalyticsHandlers{svc: svc, loc: loc}
}

// Report serves GET /api/v1/admin/analytics?from=&to=
//
// Thin, like every handler here: default the window, delegate, map, respond. It
// does no aggregation of its own — every number in the response is a column
// returned by SQL, which is the only way the reported figures and the bookings
// table cannot drift apart.
func (h *AnalyticsHandlers) Report(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")

	// Default window: the last 30 campus days, ending today. Resolved in IST
	// rather than UTC for the same reason the availability grid is — before
	// 05:30 IST a UTC "today" is yesterday, and the console would silently drop
	// the current day every morning.
	today := time.Now().In(h.loc)
	if to == "" {
		to = today.Format("2006-01-02")
	}
	if from == "" {
		from = today.AddDate(0, 0, -29).Format("2006-01-02")
	}

	rep, err := h.svc.Report(r.Context(), from, to)
	if err != nil {
		if errors.Is(err, analytics.ErrBadRange) {
			Error(w, r, fmt.Errorf("%w: %s", ErrBadRequest, err))
			return
		}
		Error(w, r, err)
		return
	}

	JSON(w, http.StatusOK, rep)
}
