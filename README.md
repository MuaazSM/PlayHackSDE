# PlayHack — Sports Facility Booking System

Booking system for IIT Guwahati's shared sports facilities (gymnasium, tennis, badminton, football, cricket). Students browse availability, pick a time slot, and get an unambiguous confirmation.

Built for **PlayHack** — Sports Board × Technical Board, IIT Guwahati — Software Development track.

## The actual problem

Not booking. **Concurrency.**

At 6 PM, when evening slots open, hundreds of students submit requests for the same court in the same few seconds. Exactly one must win, every other request must be rejected clearly and quickly, and the database must never end up in a state where availability and booking records contradict each other.

This is not a load problem — the volumes are small (~8,000 students, a few hundred bookings a day). It is a correctness problem under contention, and it is the axis this project is judged on.

## Where correctness lives

In Postgres. Not in Go, not in a lock manager, not in Redis.

```sql
CONSTRAINT no_double_book
  EXCLUDE USING gist (facility_id WITH =, during WITH &&)
  WHERE (is_exclusive AND status IN ('CONFIRMED','HELD','BLOCKED'))
```

The application performs a plain `INSERT`. Postgres serialises conflicting inserts on the GiST index and raises `23P01` for every loser, which the handler maps to `409 Conflict`. There is no read-then-write gap and no window in which application code could be wrong.

Shared facilities (the gym, capacity 30) use a single conditional `UPDATE` instead — a different problem needs a different tool.

Three commitments follow from this:

- **Correctness lives in the database.** If the answer to "where is the guarantee" is anywhere in Go, the guarantee is a bug away from disappearing.
- **Redis is never authoritative.** Eviction, restart or partition makes the system slower, never wrong.
- **Stale reads are acceptable; stale writes are not.** The availability view may briefly show a slot that has just gone. The constraint rejects the doomed booking, so the worst outcome is one wasted tap.

There is no `is_available` column anywhere in the schema. Availability is derived from the bookings table at read time — two fields that could disagree eventually will.

## Stack

| Layer | Choice |
|---|---|
| API | Go 1.22, `chi`, `pgx/v5` — direct SQL, no ORM |
| Database | PostgreSQL 16 + `btree_gist`, primary + read replica |
| Pooling | PgBouncer, transaction mode |
| Cache / bus | Redis 7 — cache, rate limits, pub/sub only |
| Frontend | Next.js 14, TypeScript, Tailwind (same build serves the PWA) |
| Realtime | Server-sent events |
| Observability | Prometheus + Grafana |

A single stateless Go binary. Microservice decomposition would be the wrong architecture at this scale.

## Quick start

```bash
make dev          # docker compose: postgres, redis, pgbouncer
make migrate-up   # apply migrations
make seed         # facilities + test users
make run          # API on :8080
```

## Tests

```bash
make test         # unit + integration
make test-race    # the concurrency suite — a required gate, never skipped
make audit        # continuous invariant checks against any database state
```

The one that matters:

```go
func TestConcurrentBooking_SingleWinner(t *testing.T) {
    // 500 goroutines, same facility, same slot, released together
    // assert: exactly 1 confirmed, 499 clean 409s
    // assert: SELECT count(*) FROM bookings WHERE ... == 1
}
```

It runs against a real Postgres via testcontainers. A mock cannot exercise an exclusion constraint, which means a mocked version of this test proves nothing about the only thing being tested.

## Race demo

```bash
make race-demo N=500
# confirmed=1  conflicts=499  db_count=1
```

Fires N concurrent attempts at one slot in-process, then reads the count back from the database. Also available as a UI surface at `/race` and as `POST /api/v1/demo/race`. No external dependency — it runs entirely against a local database.

## Scale, honestly

~8,000 students. Peak is 300–500 concurrent requests in the seconds after evening slots open. Reads beat writes roughly 100:1.

The bottleneck is contention on a single GiST index for one hot facility, which sustains write rates roughly two orders of magnitude above expected peak. The scale path — read replicas, monthly partitioning, per-facility sharding — is deliberately a plan rather than an implementation.
