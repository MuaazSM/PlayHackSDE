// Package concurrency_test is the CI gate: the race suite.
//
// It is empty until M1, because there is nothing to race yet — the write path
// (POST /api/v1/bookings) does not exist. What lands here, per CLAUDE.md and
// IMPLEMENTATION.md §15.2:
//
//   - TestConcurrentBooking_SingleWinner — 500 goroutines, one facility, one
//     slot; exactly 1 x 201 and 499 x 409, and SELECT count(*) == 1.
//   - TestIdempotentReplay — a replayed Idempotency-Key returns the original
//     booking and creates no second row.
//   - TestConcurrentCancellations_PromoteDistinct — SKIP LOCKED promotes
//     different students.
//   - TestClosureBlocksBookings — a BLOCKED window rejects bookings inside it.
//   - TestCapacityFacility_ExactlyC — N concurrent requests against capacity C
//     yield exactly C confirmations.
//
// These run against a real Postgres. A mock cannot exercise an exclusion
// constraint, so a mocked version of this suite would prove nothing.
//
// Never skip this suite to make CI faster.
package concurrency_test

import "testing"

// TestRaceSuitePending fails nothing and asserts nothing. It exists so
// `make test-race` is a working target from day one, and so the missing suite is
// visible in test output rather than silently absent.
func TestRaceSuitePending(t *testing.T) {
	t.Skip("race suite lands in M1, once POST /api/v1/bookings exists")
}
