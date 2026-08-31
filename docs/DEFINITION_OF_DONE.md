# Definition of done — verified

IMPLEMENTATION.md §19, filled in against an actual run rather than against
intent. Every number below was observed on this machine on **2026-08-31**.

The rule applied throughout: **an item is ticked only if it was observed
passing.** Anything not observed is written down as not passing, with what is
missing. A false green here surfaces on stage, in front of the people it was
meant to impress.

Environment: macOS (darwin 25.6.0, Apple silicon, 8 GB Docker VM), Go 1.22,
PostgreSQL 16 in Docker, `WRITE_QUEUE_DEPTH=24`, `DB_REPLICA_URL` empty
(availability reads fall back to the primary).

---

## Summary

| | Count |
|---|---|
| Passing, observed | 7 of 10 |
| Partially passing | 2 |
| Not passing | 1 |

The one hard failure and both partials have the **same single cause**: `/web`
contains only a `.gitkeep`. There is no frontend. Every backend guarantee this
project is actually judged on — the concurrency proof, the latency budgets, the
invariants — is green.

---

## The brief's own checklist

### 1. ☐ PARTIAL — Facilities and slots are easy to browse

*Target: campus grid loads in one request, p95 < 400 ms.*

- ✅ **One request.** `GET /api/v1/availability` returns the whole campus grid as
  a dense `[facility][slot]` array — not one request per facility. Served from
  the replica pool with a 5 s Redis cache.
- ✅ Derived at read time from the bookings table, verified against an
  independent oracle for every slot (see item 4).
- ❌ **Not browsable by a human.** There is no UI. `/web` is empty. A judge
  cannot browse anything; they can `curl` it.
- ⚠️ **p95 not measured in this pass.** The endpoint is exercised functionally by
  `test/availability` and `test/api`, but no p95 figure for the campus grid was
  recorded, so the < 400 ms target is **unverified**, not met-or-missed. The only
  latency numbers this repo can currently stand behind are the write-path ones in
  item 7.

**To close:** build the discovery grid; add a p95 assertion for
`GET /api/v1/availability` to the load harness.

---

### 2. ☐ PARTIAL — Booking confirmation is explicit

*Target: reference, facility, date, time, check-in reminder.*

Observed in `bookingResponse` (`internal/httpx/handlers.go`):

- ✅ `reference` — human-quotable booking reference
- ✅ `facility_id` **and** `facility` (the name, not just the id)
- ✅ `start` and `end` as RFC3339 `timestamptz`, localised to IST at the edge only
- ✅ `status`
- ❌ **No check-in reminder in the payload.** Check-in exists and works
  (`POST /api/v1/bookings/{id}/check-in`, HMAC venue QR, 10-minute early window,
  15-minute grace, no-show sweeper — all tested in `test/checkin`), but the
  confirmation response does not tell the student they need to do it, and there
  is no confirmation *screen* to say so either.

**To close:** add the grace deadline to the create response; build the confirm
screen.

---

### 3. ✅ PASS — Concurrent requests cannot create a double booking

*Target: `make test-race` green at N=500, enforced by the database, demonstrated
live.*

```
--- PASS: TestConcurrentBooking_SingleWinner (0.23s)
    race_test.go:80: confirmed=1 conflicts=499 db_count=1
    race_test.go:81: n=500 ok=1 err=499 spread=726.834µs elapsed=214.9275ms
                     reject_p99=214.044583ms  confirm=3.796875ms
ok  github.com/iitg-playhack/sportsbook/test/concurrency  4.440s
```

500 goroutines, 500 **distinct** users, one court, one 18:00 slot, released
together by a closed channel (start spread 727 µs). Exactly one 201, exactly 499
clean `ErrSlotTaken` — not 499 generic 500s, which would produce the same
`db_count` and still be a failure. `SELECT count(*)` read back from the database
afterwards: **1**.

**Enforced by the database, and proven so.** The constraint is not merely
present; with its predicate disabled the race test fails at 500 confirmations
(mutation test), which is what rules out the `pg_advisory_xact_lock` being the
thing doing the work. `TestExclusionConstraintStillExists` in the new audit suite
reads the live constraint definition back:

```
EXCLUDE USING gist (facility_id WITH =, during WITH &&)
  WHERE ((is_exclusive AND (status = ANY (ARRAY['CONFIRMED','HELD','BLOCKED']))))
```

Also green in the same suite: exactly C confirmations from N against capacity C
(`confirmed=30 full=170 booked=30 capacity=30 db_rows=30` at n=200), idempotent
replay, closures, and cross-facility non-contention.

⚠️ "Demonstrated live" is true via `make race-demo N=500` (CLI, in-process, no
network). The *browser* race console does not exist — see item 6.

---

### 4. ✅ PASS — Database consistent after success and failure

*Target: availability derived, never stored; `AvailabilityMatchesBookings`
passes.*

- ✅ **No `is_available` column.** Verified three ways: grep across all Go, SQL
  and migration files (zero hits outside explanatory comments); a live
  `information_schema.columns` query in the audit suite; and `make audit` green.
- ✅ **`AvailabilityMatchesBookings` passes** (`test/availability`), plus the new
  continuous audit, which runs the real read path against an independently
  written oracle:

```
--- PASS: TestINV4_AvailabilityMatchesBookings
    checking 1 date(s): [2026-08-31]
    INV-4: 110 slots agree with the bookings table
```

- ✅ All eight audit invariants green against a database in a genuinely contended
  state (a real 200-way race, a 60-way capacity burst, a closure, a live hold):

| Invariant | Result |
|---|---|
| INV-2 no overlapping CONFIRMED on an exclusive facility | PASS |
| INV-3 no shared slot with `booked > capacity`, and the counter matches the rows | PASS |
| INV-4 derived availability agrees with bookings, every slot | PASS |
| INV-5 no duplicate `(user_id, idem_key)` | PASS |
| No CONFIRMED booking overlaps a BLOCKED window | PASS |
| No orphaned HELD row past `held_until` by more than one sweep interval | PASS |
| No `is_available` column in the live catalogue | PASS |
| `no_double_book` still exists, still GiST, still `&&`, still scoped | PASS |

Run it with `make audit` (self-contained) or `make audit-live` (read-only,
against the running compose database — safe to run mid-demo).

---

### 5. ✅ PASS — Innovation addresses a real need

*Target: waitlist promotion and no-show release, both measured by M-6/M-7.*

- ✅ **Waitlist promotion is the second concurrency proof.**
  `TestConcurrentCancels_DistinctPromotions` passes: concurrent cancellations
  promote *different* students via `FOR UPDATE SKIP LOCKED`. This is
  mutation-tested twice over — plain `FOR UPDATE` also keeps them distinct, so
  there is a second test (`TestPromotion_SkipsLockedEntries`) that specifically
  goes red on that mutation, because a test that passes under the mutation is not
  testing the mechanism.
- ✅ Promotion respects priority then position; HELD blocks new bookings; HELD
  shows as held in availability; claim converts HELD → CONFIRMED; expired claims
  are rejected; the sweeper and a live cancel do not double-promote.
- ✅ **No-show release** works end to end (`test/checkin`): rotating HMAC venue
  token, 10-minute early window, 15-minute grace, sweeper marking NO_SHOW and
  releasing the slot.
- ✅ Cancel succeeds even when promotion fails — the student's cancel is never
  held hostage to the queue.

---

### 6. ❌ NOT PASSING — Demo tells a clear story from problem to proof

*Target: six beats, under four minutes, rehearsed twice.*

- ✅ The six beats are written down with exact lines, commands and per-beat
  fallbacks: `docs/DEMO_RUNBOOK.md`.
- ✅ Beats 1, 3, 5 and 6 are deliverable today. Beat 3 — the proof — is the
  strongest one and is fully in-process against a local database, so venue wifi
  cannot break it.
- ❌ **Beats 2 and 4 have no UI.** Beat 2 is "discovery grid → pick slot →
  confirm" and beat 4 is "waitlisted student promoted and notified in real time
  on a second screen". Both are implemented and tested as API and SSE; neither
  has a screen. The runbook drives them with `curl`, which is honest but is not
  the usability criterion the brief is asking about.
- ❌ **Never rehearsed.** Not once, let alone twice. No timing run exists, so
  "under four minutes" is an estimate, not a measurement.

**This is the largest single gap in the project**, and it is one gap, not three:
build `/web` and items 1, 2 and 6 all move.

---

## The internal bar

### 7. ✅ PASS — Rejection p99 < 150 ms, confirmation p99 < 250 ms

*Measured under 500-way contention, over real HTTP through the whole middleware
chain, through PgBouncer, via `make load N=500`:*

```
  500 requests in 84ms

  201      1  confirmed
  409     24  conflict — the exclusion constraint said no
  429    475  shed or rate limited

  p99 409 (reject)     80.8ms   budget 150ms   ✅  46% under
  p99 201 (confirm)    24.5ms   budget 250ms   ✅  90% under
  p99 429 (shed)       22.2ms   no budget; shedding is free by construction
  5xx                       0

  PASS
```

**Losing is faster than winning, by construction and in fact** — non-negotiable
#6. The 429 path is the fastest of the three because the shedder rejects before
the request ever reaches a connection.

The depth sweep behind `WRITE_QUEUE_DEPTH=24` is reproducible
(`TestDepthSweep_Diagnostic`), and shows the budget is a real constraint rather
than a comfortable one:

```
depth=16   admitted=16   shed=484  409_p99=10.7ms
depth=64   admitted=64   shed=436  409_p99=33.2ms
depth=128  admitted=128  shed=372  409_p99=55.4ms
depth=300  admitted=300  shed=200  409_p99=137.4ms
depth=500  admitted=500  shed=0    409_p99=218.1ms   <- over budget
```

Unshed, all 500 admitted, the reject p99 is 218 ms and misses. The shedder is
load-bearing, not decoration.

---

### 8. ✅ PASS — Redis flushed mid-run: still correct, only slower

*Non-negotiable #3, now executable: `test/chaos/chaos_test.go`.*

```
--- PASS: TestRedisFlushMidRun (3.92s)
    redis flushed 77 times mid-run; outcome:
    {created:1 conflict:130 shed:69 other4xx:0 server5xx:0 transport:0}
```

`FLUSHALL` fired 77 times during 200 concurrent bookings — repeatedly rather than
once, so it cannot land in a gap and prove nothing. Result: **zero 5xx**, zero
transport errors, exactly one winner, `db_count = 1`. Nothing on the request path
needs Redis to be there.

Three further chaos scenarios, all green:

| Scenario | Result |
|---|---|
| `TestStaleAvailabilityDoesNotCorruptWrites` — availability grid frozen all-free for the whole storm | PASS, `db_count=1`, 0 5xx |
| `TestAPIRestartMidRace` — connections severed mid-flight, fresh API on the same rows, 400 total attempts | PASS, `db_count=1`; the restarted API correctly handed out **zero** winners |
| `TestPgBouncerRestart` — every backend terminated mid-storm | PASS, `db_count=1`, no booking without its audit event, pool self-recovered |

⚠️ **Two honest caveats on the chaos suite.**

1. `TestReplicaLagDoesNotCorrupt` **SKIPS** and is not counted as passing. There
   is no streaming replica in this environment — `make dev-replica` has never
   been brought up — so there was nothing to pause. It skips with a printed
   reason rather than being faked.
   `TestStaleAvailabilityDoesNotCorruptWrites` covers the property (a lying read
   path cannot cause a wrong booking) by a route that does exist.
2. `TestPgBouncerRestart` does **not** restart the PgBouncer container. The suite
   runs against a testcontainer Postgres with no pooler in front of it, so it
   reproduces what a pooler restart looks like from the application's side —
   every checked-out connection dying at once. It therefore does **not** cover
   loss of server-side prepared statements or session state. The test says so in
   its own doc comment.
   14 requests returned 5xx in that run (38 backends terminated) and the test
   deliberately does not assert them away: a request whose connection dies
   mid-transaction has genuinely failed, and 500 is the correct answer. Retrying
   inside the handler is how one lost booking becomes two.

---

### 9. ✅ PASS — Every SQLSTATE inspection lives in `store/pgerr.go`

- ✅ Repo-wide grep for five-character SQLSTATE literals: the only matches under
  `internal/` and `cmd/` are in `internal/store/pgerr.go` (`23P01`, `23505`,
  `40P01`). Elsewhere they appear only inside test assertions and comments.
- ✅ Enforced by a test, not by a convention:
  `TestNoSQLSTATEOutsidePgerr` walks `internal/` and `cmd/` and fails on any
  literal outside that one file. Green.
- ✅ `TestNoInlineSQLOutsideQueries` also green — write-path SQL lives in `.sql`
  files.

---

### 10. ✅ PASS — No `is_available` column exists anywhere in the schema

- ✅ Grep across `*.go`, `*.sql`, `*.ts`, `*.tsx` and all ten migrations: zero
  occurrences outside comments that explain why it is absent.
- ✅ `TestNoIsAvailableColumn` queries `information_schema.columns` on the live
  database for `column_name ILIKE '%is_available%'` and requires zero rows.
  Green. A grep proves nobody wrote it into a migration; this proves nobody added
  it by hand.

---

## Additional static sweep

Run as part of this hardening pass. All clean, no fixes were required.

| Check | Result |
|---|---|
| SQLSTATE strings appear only in `store/pgerr.go` | ✅ clean |
| No `is_available` anywhere, in code or migrations | ✅ clean |
| No SELECT-then-INSERT on any booking path | ✅ clean — enforced at runtime by `TestCreate_NeverReadsOccupancy`, which records the statements actually issued rather than reading the source |
| Every `tstzrange` construction uses `'[)'` | ✅ clean — all 33 call sites across `internal/` and `test/` |
| `go vet ./...` | ✅ clean |
| `golangci-lint` | ⚠️ **not run — not installed on this machine** |

---

## Known gaps, consolidated

Ordered by what a judge would notice first.

1. **No frontend.** `/web` is a `.gitkeep`. Blocks DoD items 1, 2 and 6.
2. **Demo never rehearsed.** No timing run; "under four minutes" is an estimate.
3. **Read replica never brought up.** `make dev-replica` is untested;
   `DB_REPLICA_URL` empty and falling back to the primary is a supported
   configuration and is how everything here is run and tested — but the standby
   itself has not been exercised, and neither has replica lag.
4. **Analytics endpoints not implemented.** Phase 14 was skipped as P2.
   `GET /api/v1/admin/analytics` (§10.2) does not exist. The README does not
   claim it.
5. **`testutil.Slot18()` pins to *today* 18:00 IST.** Any suite using it fails
   when run after 18:00 local time, for reasons unrelated to correctness.
6. **Full suite is flaky at default parallelism.** `go test ./... -race` runs
   every package concurrently, each with its own Postgres container; under that
   load the latency-budgeted tests in `test/booking` and `test/concurrency`
   intermittently miss their budgets. Two such failures were observed during this
   pass (`TestAlternatives_ExcludesClosedAndInactive`,
   `TestConflictLatency_Under150ms`); both passed in isolation on re-run, and
   both were caused by this hardening pass itself — the new `test/chaos` and
   `test/invariants` packages each start their own Postgres and fire 200-request
   storms, and were initially running inside `make test`. They are now skipped
   under `-short` and have their own targets (`make chaos`, `make audit`), which
   restores `make test` to its previous weight. **Use `-p 4` for the full
   suite.** No existing test was modified or weakened.
7. **`golangci-lint` not installed**, so `make lint` was not run in this pass.
8. **`internal/store/queries/waitlist_head_for_update.sql` is dead code**,
   superseded by `waitlist_claim_head.sql`, retained because an existing test
   asserts its constant.

---

## Reproducing this document

```bash
make dev            # postgres, pgbouncer, redis, prometheus, grafana
make migrate-up
make seed

make test           # unit + integration (-short; excludes chaos and audit)
make test-race      # the concurrency gate — item 3
make load N=500     # the latency gate — item 7
make chaos          # the chaos suite — item 8
make audit          # the invariant suite — item 4

go test ./... -race -count=1 -p 4    # everything, at safe parallelism
```
