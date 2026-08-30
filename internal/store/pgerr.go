package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// THIS IS THE ONLY FILE IN THE REPOSITORY THAT INSPECTS A SQLSTATE STRING.
//
// Nothing in booking/, waitlist/, httpx/ or anywhere else may match on "23P01",
// "23505" or a constraint name. They match on the sentinels below with
// errors.Is. If a new database error needs a meaning, it gets one here.
//
// The mapping is IMPLEMENTATION.md §4.5:
//
//   SQLSTATE  Constraint              Domain error          HTTP
//   23P01     no_double_book          ErrSlotTaken          409 SLOT_TAKEN
//   23505     uq_bookings_user_idem   ErrIdempotentReplay   200 + original
//   23505     uq_waitlist_live        ErrAlreadyWaiting     409 ALREADY_WAITING
//   40P01     —                       ErrDeadlock           retried, then mapped
//   0 rows    capacity_take           ErrCapacityFull       409 CAPACITY_FULL
//   —         policy                  ErrPolicyExceeded     422 POLICY_LIMIT
//   —         shed                    ErrShed               429 + Retry-After
//   —         deadline exceeded       ErrTimeout            503
// ---------------------------------------------------------------------------

// SQLSTATE codes. Private on purpose — they must not leak to callers.
const (
	codeExclusionViolation = "23P01"
	codeUniqueViolation    = "23505"
	codeDeadlockDetected   = "40P01"
)

// Constraint names, as created in migrations 0003 and 0005. These are part of
// the contract between the schema and this file.
const (
	constraintNoDoubleBook = "no_double_book"
	constraintBookingsIdem = "uq_bookings_user_idem"
	constraintWaitlistLive = "uq_waitlist_live"
)

// Domain errors. Callers match these with errors.Is.
var (
	// ErrSlotTaken means another booking already occupies the slot. The
	// exclusion constraint decided this, not application code.
	ErrSlotTaken = errors.New("slot already taken")

	// ErrIdempotentReplay means this (user, idempotency key) already produced a
	// booking. Roll back, re-read the original, return it with 200.
	ErrIdempotentReplay = errors.New("idempotent replay")

	// ErrAlreadyWaiting means the user already holds a live waitlist entry for
	// this facility and slot.
	ErrAlreadyWaiting = errors.New("already on the waitlist")

	// ErrCapacityFull means a shared facility's counter is at its cap. Raised
	// by the caller when capacity_take returns zero rows, not by a SQLSTATE.
	ErrCapacityFull = errors.New("capacity full")

	// ErrPolicyExceeded means a fair-use cap was hit. Advisory — see §4.7.
	ErrPolicyExceeded = errors.New("policy limit exceeded")

	// ErrShed means the write queue was full and the request was rejected
	// immediately rather than queued. Losing fast is the feature.
	ErrShed = errors.New("request shed")

	// ErrTimeout means the request outlived its usefulness.
	ErrTimeout = errors.New("timeout")

	// ErrNotFound means a row that was expected to exist did not.
	ErrNotFound = errors.New("not found")

	// ErrDeadlock means Postgres chose this transaction as the victim to break a
	// lock cycle and rolled it back. Nothing was written.
	//
	// This is reachable on the booking path for a structural reason. When two
	// transactions insert overlapping rows, each places its index tuple before
	// scanning for conflicts, so each can end up waiting on the other while
	// checking the exclusion constraint. Postgres reports exactly that:
	//
	//	while checking exclusion constraint on tuple (14,2) in relation "bookings"
	//
	// Unlike a conflict, a deadlock carries no verdict — the transaction was
	// aborted arbitrarily, not because it lost the slot. The caller may retry it.
	ErrDeadlock = errors.New("deadlock detected")
)

// Error is a classified database error. It wraps both the domain sentinel and
// the underlying driver error, so callers can match the meaning with errors.Is
// and still recover the original with errors.As if they need to log it.
type Error struct {
	Kind       error  // one of the sentinels above
	Code       string // SQLSTATE, empty when not a Postgres error
	Constraint string // constraint that raised it, empty when not applicable
	cause      error
}

func (e *Error) Error() string {
	switch {
	case e.Constraint != "":
		return fmt.Sprintf("%s (sqlstate %s, constraint %s)", e.Kind, e.Code, e.Constraint)
	case e.Code != "":
		return fmt.Sprintf("%s (sqlstate %s)", e.Kind, e.Code)
	default:
		return e.Kind.Error()
	}
}

// Unwrap returns both the sentinel and the cause, so errors.Is matches the
// domain meaning and errors.As still reaches *pgconn.PgError.
func (e *Error) Unwrap() []error {
	if e.cause == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.cause}
}

// Classify turns a driver error into a domain error.
//
// Matching is on SQLSTATE *and* constraint name together. Two different 23505s
// mean two different things — an idempotent replay is a success the client
// should see as 200, a duplicate waitlist entry is a 409 — and collapsing them
// into one meaning would return the wrong thing to a real user.
//
// Anything unrecognised is wrapped and returned unclassified. It must not be
// guessed at: an unclassified error is a 500, and a 500 is the honest answer
// when the system does not know what happened.
func Classify(err error) error {
	if err == nil {
		return nil
	}

	// A cancelled or expired context is not a database condition.
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrTimeout, cause: err}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &Error{Kind: ErrNotFound, cause: err}
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("store: unclassified: %w", err)
	}

	switch pgErr.Code {
	case codeDeadlockDetected:
		return newError(ErrDeadlock, pgErr)
	case codeExclusionViolation:
		if pgErr.ConstraintName == constraintNoDoubleBook {
			return newError(ErrSlotTaken, pgErr)
		}
	case codeUniqueViolation:
		switch pgErr.ConstraintName {
		case constraintBookingsIdem:
			return newError(ErrIdempotentReplay, pgErr)
		case constraintWaitlistLive:
			return newError(ErrAlreadyWaiting, pgErr)
		}
	}

	// Right family, wrong constraint — or a code we have no meaning for. Do not
	// guess.
	return fmt.Errorf("store: unclassified: %w", err)
}

func newError(kind error, pgErr *pgconn.PgError) *Error {
	return &Error{
		Kind:       kind,
		Code:       pgErr.Code,
		Constraint: pgErr.ConstraintName,
		cause:      pgErr,
	}
}

// IsClassified reports whether Classify recognised the error. Useful for
// deciding between a mapped status and a 500.
func IsClassified(err error) bool {
	var e *Error
	return errors.As(err, &e)
}
