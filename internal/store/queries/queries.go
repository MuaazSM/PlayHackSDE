// Package queries embeds every .sql file in this directory and exposes them by
// name.
//
// All SQL lives in .sql files. Nothing inlines a query mid-function: the
// concurrency mechanism is a specific SQL construct, and it should be readable
// as SQL rather than reassembled from string fragments.
//
// The loader validates at init. A missing or misnamed file is a boot failure,
// not a runtime surprise on the one request that happens to need it.
package queries

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Named queries. Every constant here must have a matching <name>.sql file or
// the package refuses to load.
const (
	BookingInsertExclusive = "booking_insert_exclusive"
	BookingInsertShared    = "booking_insert_shared"
	BookingLockFacility    = "booking_lock_facility"
	BookingFindByIdem      = "booking_find_by_idem"
	BookingGet             = "booking_get"
	BookingCancel          = "booking_cancel"
	BookingListMine        = "booking_list_mine"
	UserRole               = "user_role"
	BookingEventInsert     = "booking_event_insert"
	CapacityTake           = "capacity_take"
	CapacityRelease        = "capacity_release"
	BookingInsertHeld      = "booking_insert_held"
	BookingClaimHeld       = "booking_claim_held"
	BookingExpireHeld      = "booking_expire_held"

	// Check-in and automatic release (§7).
	CheckinRedeem           = "checkin_redeem"
	CheckinGet              = "checkin_get"
	BookingMarkNoShow       = "booking_mark_no_show"
	BookingCompleteAttended = "booking_complete_attended"

	// Manager closures (§10.4). A closure is a booking row, so these sit beside
	// the booking queries rather than in a namespace of their own.
	ClosureInsert          = "closure_insert"
	ClosureFind            = "closure_find"
	ClosureGet             = "closure_get"
	ClosureList            = "closure_list"
	ClosureAffected        = "closure_affected"
	ClosureReopen          = "closure_reopen"
	ClosureZeroCapacity    = "closure_zero_capacity"
	ClosureRestoreCapacity = "closure_restore_capacity"

	// Fair-use policy and priority tiers (§11). Advisory by design — see
	// internal/policy and IMPLEMENTATION.md §4.7.
	PolicyResolve  = "policy_resolve"
	PolicyUsage    = "policy_usage"
	PolicyPriority = "policy_priority"

	WaitlistHeadForUpdate = "waitlist_head_for_update"
	WaitlistClaimHead     = "waitlist_claim_head"
	WaitlistJoin          = "waitlist_join"
	WaitlistLeave         = "waitlist_leave"
	WaitlistPlace         = "waitlist_place"
	WaitlistPromote       = "waitlist_promote"
	WaitlistMarkClaimed   = "waitlist_mark_claimed"
	WaitlistMarkExpired   = "waitlist_mark_expired"
	FacilityGet           = "facility_get"
	FacilityList          = "facility_list"
	AvailabilityFacility  = "availability_facility_day"
	AvailabilityShared    = "availability_shared_day"
	AvailabilityCampus    = "availability_campus_day"
	AlternativesSameFacil = "alternatives_same_facility"
	AlternativesSameSport = "alternatives_same_sport"
	OutboxInsert          = "outbox_insert"
	OutboxDrain           = "outbox_drain"
	OutboxMarkFailed      = "outbox_mark_failed"
	OutboxRequeueFailed   = "outbox_requeue_failed"
	OutboxListen          = "outbox_listen"

	// The race console (§13). DemoCountConfirmed is the proof; the rest is
	// stage management.
	DemoCountConfirmed = "demo_count_confirmed"
	DemoResetSlot      = "demo_reset_slot"
	DemoBookers        = "demo_bookers"
	DemoWinner         = "demo_winner"
)

// required is checked against the embedded files at init.
var required = []string{
	BookingInsertExclusive,
	BookingInsertShared,
	BookingLockFacility,
	BookingFindByIdem,
	BookingGet,
	BookingCancel,
	BookingListMine,
	UserRole,
	BookingEventInsert,
	CapacityTake,
	CapacityRelease,
	BookingInsertHeld,
	BookingClaimHeld,
	BookingExpireHeld,
	CheckinRedeem,
	CheckinGet,
	BookingMarkNoShow,
	BookingCompleteAttended,
	ClosureInsert,
	ClosureFind,
	ClosureGet,
	ClosureList,
	ClosureAffected,
	ClosureReopen,
	ClosureZeroCapacity,
	ClosureRestoreCapacity,
	PolicyResolve,
	PolicyUsage,
	PolicyPriority,
	WaitlistHeadForUpdate,
	WaitlistClaimHead,
	WaitlistJoin,
	WaitlistLeave,
	WaitlistPlace,
	WaitlistPromote,
	WaitlistMarkClaimed,
	WaitlistMarkExpired,
	FacilityGet,
	FacilityList,
	AvailabilityFacility,
	AvailabilityShared,
	AvailabilityCampus,
	AlternativesSameFacil,
	AlternativesSameSport,
	OutboxInsert,
	OutboxDrain,
	OutboxMarkFailed,
	OutboxRequeueFailed,
	OutboxListen,
	DemoCountConfirmed,
	DemoResetSlot,
	DemoBookers,
	DemoWinner,
}

var loaded map[string]string

func init() {
	m, err := load(files, required)
	if err != nil {
		// Deliberate panic. A missing query is a build/packaging mistake, and
		// failing at boot is strictly better than failing on the request that
		// needs it — which, for the booking path, is the demo.
		panic("store/queries: " + err.Error())
	}
	loaded = m
}

// load reads every .sql file in fsys and verifies each required name is present.
//
// Split out from init so the failure path is testable without crashing the
// package that owns it.
func load(fsys fs.FS, required []string) (map[string]string, error) {
	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("globbing *.sql: %w", err)
	}

	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		body, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry, err)
		}
		name := strings.TrimSuffix(path.Base(entry), ".sql")

		if strings.TrimSpace(stripComments(string(body))) == "" {
			return nil, fmt.Errorf("query %q is empty", name)
		}
		out[name] = string(body)
	}

	var missing []string
	for _, name := range required {
		if _, ok := out[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		have := make([]string, 0, len(out))
		for name := range out {
			have = append(have, name)
		}
		sort.Strings(have)
		return nil, fmt.Errorf("missing required queries %v (have %v)", missing, have)
	}

	return out, nil
}

// stripComments removes leading -- comment lines so a file containing only
// documentation counts as empty.
func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Get returns the SQL registered under name.
//
// It panics on an unknown name. Every call site passes a constant from this
// package, so an unknown name is a programming error that init should already
// have caught — returning an error here would only push a compile-time mistake
// into runtime error handling.
func Get(name string) string {
	q, ok := loaded[name]
	if !ok {
		panic(fmt.Sprintf("store/queries: unknown query %q", name))
	}
	return q
}

// Names lists every loaded query, sorted. Useful in diagnostics.
func Names() []string {
	out := make([]string, 0, len(loaded))
	for name := range loaded {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LoadForTest exposes the loader so its failure path can be tested. The real
// load happens in init, which by definition cannot be exercised from a test that
// needs the package to have loaded successfully.
func LoadForTest(fsys fs.FS, required []string) (map[string]string, error) {
	return load(fsys, required)
}
