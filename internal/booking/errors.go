package booking

import (
	"errors"
	"fmt"
)

// Domain errors for the write path.
//
// These are the booking package's own vocabulary, deliberately separate from the
// store package's sentinels. store says what the database did; booking says what
// it means for a user. httpx maps these to status codes and never sees a
// SQLSTATE or a store error.
var (
	// ErrSlotTaken means another confirmed booking or closure already occupies
	// the window. Decided by the exclusion constraint, never by application
	// code. Maps to 409 SLOT_TAKEN.
	ErrSlotTaken = errors.New("slot already taken")

	// ErrCapacityFull means a shared facility is at its cap for the slot.
	// Maps to 409 CAPACITY_FULL.
	ErrCapacityFull = errors.New("capacity full")

	// ErrIdempotentReplay means this (user, idempotency key) already produced a
	// booking. Callers of Create do not normally see this: Create resolves the
	// replay and returns the original booking with Replayed set. Maps to 200.
	ErrIdempotentReplay = errors.New("idempotent replay")

	// ErrPolicyExceeded means a fair-use cap was hit. Advisory — see §4.7.
	// Maps to 422 POLICY_LIMIT.
	ErrPolicyExceeded = errors.New("policy limit exceeded")

	// ErrShed means the write queue was full and the request was rejected
	// immediately rather than queued. Maps to 429 with Retry-After.
	ErrShed = errors.New("request shed")

	// ErrNotFound means a referenced facility or booking does not exist.
	// Maps to 404.
	ErrNotFound = errors.New("not found")

	// ErrForbidden means the caller may not act on this booking. Maps to 403.
	ErrForbidden = errors.New("forbidden")

	// ErrNotCancellable means the booking exists but was not in a cancellable
	// state — already cancelled, or already completed. Maps to 409.
	//
	// Distinct from ErrNotFound on purpose: a double cancel must not report
	// success, and it must not claim the booking never existed either.
	ErrNotCancellable = errors.New("booking is not cancellable")

	// ErrOfferExpired means a waitlist promotion offer could not be claimed:
	// the claim window closed, the sweeper reclaimed the court, or the booking
	// was never a live hold. Maps to 409.
	//
	// Distinct from ErrNotCancellable because the student did nothing wrong and
	// the useful next action is different — they should re-join the queue, not
	// look for a booking that is not there.
	ErrOfferExpired = errors.New("promotion offer is no longer claimable")

	// ErrValidation means the request was malformed or out of bounds. Maps to
	// 422. Use ValidationError to carry the offending field.
	ErrValidation = errors.New("validation failed")
)

// policyError carries a fair-use refusal from internal/policy into this
// package's vocabulary without flattening it.
//
// Two unwrap targets, the same shape store.Error uses: errors.Is finds
// ErrPolicyExceeded so httpx maps the status without importing the policy
// package's sentinel, and errors.As still reaches *policy.LimitError so the 422
// body can name the limit and when it resets. Collapsing it to a bare sentinel
// would lose exactly the two fields that make the response actionable.
type policyError struct{ cause error }

func (e *policyError) Error() string { return e.cause.Error() }

func (e *policyError) Unwrap() []error { return []error{ErrPolicyExceeded, e.cause} }

// ValidationError names the field that failed, so the API can point the user at
// the control that needs fixing instead of returning a wall of prose.
type ValidationError struct {
	Field string
	// Code is the stable machine-readable error code the API returns, e.g.
	// SLOT_NOT_ALIGNED. Empty means the generic validation code.
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Unwrap makes errors.Is(err, ErrValidation) true for every validation failure,
// so callers can handle the class without enumerating fields.
func (e *ValidationError) Unwrap() error { return ErrValidation }

// invalid builds a ValidationError.
func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Field returns the offending field name, or "" if err is not a validation
// error.
func Field(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return ""
}

// Code returns the machine-readable validation code, or "" if err is not a
// validation error or carries no specific code.
func Code(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}
