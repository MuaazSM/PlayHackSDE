// Package concurrency_test is the CI gate: the race suite.
//
// It runs against a real Postgres. A mock cannot exercise an exclusion
// constraint, so a mocked version of these tests would prove nothing about the
// only thing being tested.
//
// Landed:
//   - TestConcurrentBooking_SingleWinner — 500 goroutines, one slot, 1/499/db=1
//   - TestCreate_DifferentFacilitiesDoNotContend — no cross-facility interference
//
// Still to land, per IMPLEMENTATION.md §15.2:
//   - SharedCapacity_ExactlyC — N against capacity C yields exactly C (Phase 3)
//   - ConcurrentCancels_DistinctPromotions — SKIP LOCKED promotes different users
//   - ClosureBlocksBooking — a BLOCKED window rejects bookings inside it
//   - NoShowRelease — past grace with no check-in releases the slot
//   - OutboxNeverPrecedesCommit — forced rollback leaves zero outbox rows
//
// Never skipped to make CI faster.
package concurrency_test
