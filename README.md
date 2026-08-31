# PlayHack — Sports Facility Booking System

Booking system for IIT Guwahati's shared sports facilities (gymnasium, tennis, badminton, football, cricket), built for **PlayHack** — Sports Board × Technical Board — Software Development track. Students browse availability, pick a time slot, and get an unambiguous confirmation. But the actual problem here is not booking, it is **concurrency**: at 6 PM when evening slots open, hundreds of students submit requests for the same court within the same few seconds, exactly one must win, every other request must be rejected clearly and quickly, and the database must never end up in a state where availability and booking records contradict each other. The volumes are small — roughly 8,000 students and a few hundred bookings a day — so this is not a load problem. It is a correctness problem under contention, and everything else in this codebase exists to give that guarantee something to be correct about.

---

## 5-minute setup

Requires Docker (for the compose stack) and Go 1.22+. No other host dependency: `golang-migrate` runs straight from the module deps if the CLI is not installed.

```bash
make dev          # docker compose: postgres, pgbouncer, redis, prometheus, grafana
make migrate-up   # apply migrations 0001-0010
make seed         # 7 facilities, 12 users, 1 global policy (idempotent)
make run          # API on :8080
```

Then, in a second terminal:

```bash
make race-demo N=500     # fire 500 concurrent attempts at one slot
# confirmed=1 conflicts=499 other=0 db_count=1
```

**Host-port overrides.** The Makefile documents three, and you need them if you already run PostgreSQL or Redis on the host — a host-installed server binds `127.0.0.1` first and silently wins over Docker's wildcard bind, so `localhost:5432` would reach the wrong database and the failure looks like missing tables rather than a port collision:

```bash
POSTGRES_HOST_PORT=55432 PGBOUNCER_HOST_PORT=56432 REDIS_HOST_PORT=56379 make dev
```

Pass the same overrides to every subsequent target (`make migrate-up`, `make seed`, `make run`) — they feed the connection strings, not just the compose file.

**`make migrate-up` goes direct to Postgres, not through PgBouncer.** `golang-migrate` takes a *session* advisory lock, which a transaction-mode pooler cannot honour. The Makefile therefore uses a separate `DB_MIGRATE_URL` pointed at `POSTGRES_HOST_PORT`. `DB_LISTEN_URL` bypasses the pooler for the same family of reason: the outbox dispatcher holds a `LISTEN`, which is session state, and through PgBouncer the subscription looks established and then quietly receives nothing.

### Other targets that exist

| Target | What it does |
|---|---|
| `make dev-replica` | Additionally brings up the streaming standby on :5433 (opt-in profile — see Known limitations) |
| `make down` / `make logs` | Stop the stack (keeps the volume) / tail compose logs |
| `make migrate-down` / `make migrate-drop` | Roll back one migration / roll back to a clean database |
| `make worker` | Outbox dispatcher + sweepers as a separate process (not needed for the demo — `make run` embeds them, `EMBED_WORKERS` defaults to true) |
| `make test` | Unit + integration, `-short` |
| `make test-race` | The concurrency suite. Slow. Do not skip. |
| `make race-demo N=500` | Fire N concurrent requests, print the outcome split |
| `make race-reset` | Clear the demo slot without racing |
| `make load N=500` | The 6 PM surge as a pass/fail gate — exits non-zero if p99(409) ≥ 150 ms, p99(201) ≥ 250 ms, or anything returned 5xx |
| `make chaos` | Break Redis, the read path, the API and the connection pool mid-race, and check the invariant survived each one |
| `make audit` | Continuous-invariant suite against a throwaway Postgres, seeded with real contended state first (a clean database satisfies every invariant trivially) |
| `make audit-live` | The same suite pointed at the running compose database. Read-only, safe mid-demo |
| `make psql` | psql into the compose Postgres |
| `make lint` / `make tidy` | golangci-lint / go mod tidy |

`WRITE_QUEUE_DEPTH` defaults to 24 in both the application and Makefile. It is a per-environment number by construction: through PgBouncer against the compose stack a booking transaction costs several times more than on the tuned container, and at 128 this hardware misses the 150 ms rejection budget outright (368 ms). The sweep table that produced 24 is in the Makefile.

---

## Architecture

```
   ┌──────────────┐    ┌──────────────┐
   │ Browser /PWA │    │  Race console│         (implemented in /web)
   └──────┬───────┘    └──────┬───────┘
          │                   │
          ▼                   ▼
   ┌──────────────────────────────────┐
   │  Next.js 14 (App Router, TS)     │  /web
   └──────────────┬───────────────────┘
                  │ HTTP + SSE
                  ▼
   ┌──────────────────────────────────────────────────────────┐
   │  chi API — one stateless Go binary, N replicas  :8080    │
   │                                                          │
   │  RequestID → Recover → Metrics → CORS → RateLimit(IP)    │
   │    → Auth → RateLimit(user) → Idempotency                │
   │    → Shed → Timeout → handler                            │
   │                                                          │
   │  ┌────────────┐  ┌───────────┐  ┌────────────────────┐   │
   │  │ booking    │  │ facility  │  │ waitlist / checkin │   │
   │  │ write path │  │ read path │  │ closures / policy  │   │
   │  └────────────┘  └───────────┘  └────────────────────┘   │
   │                                                          │
   │  ┌──────────────────────┐   ┌──────────────────────┐     │
   │  │ outbox dispatcher    │   │ live hub → SSE       │     │
   │  │ (LISTEN outbox_new)  │   │ GET /api/v1/stream   │     │
   │  └──────────┬───────────┘   └──────────▲───────────┘     │
   └─────────────┼──────────────────────────┼─────────────────┘
                 │                          │
       writes ───┼──────────┐               │ pub/sub  slots:{date}
                 │          │               │ cache    avail:{date}
                 │          │               │ ratelimit rl:ip / rl:user
                 │          │        ┌──────┴───────────┐
                 │          │        │   Redis 7        │  never authoritative
                 │          │        └──────────────────┘
                 ▼          │
   ┌──────────────────────┐ │  LISTEN + migrations bypass the pooler
   │ PgBouncer (txn mode) │ │  (session state; ~25 backend conns)
   └──────────┬───────────┘ │
              │             │
              ▼             ▼
   ┌────────────────────────────┐     streaming      ┌──────────────────────┐
   │  PostgreSQL 16 (primary)   │ ─────────────────▶ │  read replica :5433  │
   │  btree_gist                │    replication     │  availability reads  │
   │  EXCLUDE ... no_double_book│                    │  (opt-in; falls back │
   │  slot_capacity, outbox     │                    │   to primary)        │
   └────────────────────────────┘                    └──────────────────────┘
```

Prometheus (:9090) scrapes `/metrics`; Grafana (:3001) serves the surge dashboard at `/d/playhack-surge`. Both come up with the default `make dev`.

---

## The mechanism, in one paragraph

Exclusive facilities — a tennis court, a badminton court, a football field — use **Mechanism A**: a Postgres exclusion constraint, `EXCLUDE USING gist (facility_id WITH =, during WITH &&) WHERE (is_exclusive AND status IN ('CONFIRMED','HELD','BLOCKED'))`, over a `tstzrange` built with `'[)'` bounds so 18:00–19:00 and 19:00–20:00 do not collide. The handler issues a plain `INSERT` — there is no `SELECT` to check availability first, because that gap *is* the bug — and Postgres raises `23P01` for every loser, which `internal/store/pgerr.go` maps to `409 Conflict`. Shared facilities, meaning the gymnasium at capacity 30, use **Mechanism B** instead: a single conditional statement, `UPDATE slot_capacity SET booked = booked + 1 WHERE facility_id = $1 AND slot_start = $2 AND booked < capacity RETURNING booked`, where zero rows affected means full. Both run inside the same transaction as the booking insert, selected on `facilities.is_exclusive`. The winner of a race is therefore decided by a database constraint and never by Go: no mutex, no service-layer check, no Redis lock — because if the answer to "where is the guarantee" lives in application code, the guarantee is one refactor away from disappearing, and there is no `is_available` column anywhere in the schema for the same reason (two fields that could disagree eventually will). The write path does take a `pg_advisory_xact_lock` per facility as the first statement of the transaction, but that is a **contention shaper, not the concurrency control** — without it, concurrent overlapping inserts each place their GiST index tuple before scanning for conflicts and deadlock inside the constraint check itself (measured: ~170 of 500 requests aborted as deadlock victims); with the constraint's predicate disabled the race test fails at 500 confirmations, which is the mutation proof that correctness comes from the constraint and not the lock.

---

## Endpoints

Ground truth is `internal/httpx/router.go`. Everything below exists in the code; nothing that does not is listed.

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/healthz` | — | Liveness. Outside auth *and* outside rate limiting |
| GET | `/readyz` | — | DB required; Redis degraded does not make a replica unready |
| GET | `/metrics` | — | Prometheus |
| POST | `/api/v1/dev/login` | — | **Registered only when `AUTH_MODE=dev`.** In oidc mode the route does not exist |
| GET | `/api/v1/stream` | any | SSE. Accepts `?access_token=` because EventSource cannot set headers — scoped to this route only. Not shed, not timed out |
| GET | `/api/v1/facilities` | any | Catalogue |
| GET | `/api/v1/facilities/{id}/availability` | any | Derived from bookings; replica + 5 s Redis cache |
| GET | `/api/v1/availability` | any | Campus grid, one request |
| POST | `/api/v1/bookings` | student | **The write path.** `Idempotency-Key` required. The only shed route, and the only one with a write timeout |
| GET | `/api/v1/bookings/me` | student | Upcoming + past |
| DELETE | `/api/v1/bookings/{id}` | owner | Cancel → release → promote |
| POST | `/api/v1/bookings/{id}/claim` | owner | Accept a promotion offer (HELD → CONFIRMED) |
| POST | `/api/v1/bookings/{id}/check-in` | owner | Redeem the venue QR |
| POST | `/api/v1/waitlist` | student | Returns queue position. Exclusive facilities only |
| DELETE | `/api/v1/waitlist/{id}` | owner | Leave |
| GET | `/api/v1/facilities/{id}/checkin-token` | **manager** | Mints the rotating venue QR payload |
| POST | `/api/v1/closures` | **manager** | Inserts a `BLOCKED` row. `Idempotency-Key` required |
| GET | `/api/v1/closures` | **manager** | Console board; names affected bookings |
| DELETE | `/api/v1/closures/{id}` | **manager** | Reopen; restores declared capacity |
| GET | `/api/v1/admin/analytics` | **manager or secretary** | Utilisation, peak-demand heatmap, no-show rate, unmet demand, slot recovery. `?from=&to=` local dates, inclusive. Derived by query; replica + 60 s Redis cache |
| POST | `/api/v1/demo/race` | any | **Dev mode only.** N in-process attempts + a fresh DB read-back |
| POST | `/api/v1/demo/reset` | any | **Dev mode only.** Clears the demo slot so the race re-runs on stage |

Rate limiting is split around auth on purpose: a coarse IP bucket first so unauthenticated floods are cheap to reject, then a per-user bucket once we know who this is — limiting only after auth would spend a JWT verification on every request of a flood, and limiting only before it would bucket everyone behind one campus NAT address together. Reads are never shed: availability is cheap and cacheable, and serving it during a burst is what keeps the screen honest about who won.

Every non-2xx uses one envelope (`error` code, human `message`, plus `alternatives`, `waitlist_available`, `conflicts` and `request_id` where meaningful). The client never parses prose.

---

## Demo runbook

Six beats, 3 min 20 s. Full version with pre-flight checklist, exact lines and per-beat fallbacks: **[docs/DEMO_RUNBOOK.md](docs/DEMO_RUNBOOK.md)**.

| # | Beat | Time | Action |
|---|---|---|---|
| 1 | Problem | 20 s | Two students, one court, 6 PM. What happens today. |
| 2 | Product | 60 s | Discovery grid → pick slot → confirm. Deliberately unhurried; this is the usability criterion. |
| 3 | **Proof** | 45 s | Race console. Fire 500. 1 green, 499 red. `SELECT count(*) → 1`, live. Fire again. Still 1. |
| 4 | Recovery | 30 s | Cancel the winner. Waitlisted student promoted and notified in real time on a second screen. |
| 5 | Mechanism | 30 s | One slide: the exclusion constraint, and why it beats locks, version columns and Redis. |
| 6 | Path | 15 s | "The bottleneck is one GiST index on one hot facility, ~100× above our peak." Close. |

The race console runs fully in-process against the local database. Venue wifi cannot break it.

---

## Tests

```bash
make test         # unit + integration, -short
make test-race    # the concurrency suite — a required gate, never skipped
make load N=500   # the surge, as a pass/fail latency gate
make chaos        # Redis wiped, API killed, every connection dropped, mid-race
make audit        # invariant checks against contended state
```

`make chaos` and `make audit` are excluded from `make test` (which runs `-short`)
because each starts its own Postgres and fires a 200-request storm; running them
alongside every other package saturates a laptop Docker VM badly enough to knock
over the latency-budgeted tests elsewhere. They are separate targets, not skipped
tests — both are required gates.

**A verified definition-of-done, with real numbers and the gaps written down as
gaps, is in [docs/DEFINITION_OF_DONE.md](docs/DEFINITION_OF_DONE.md).**

The one that matters:

```go
func TestConcurrentBooking_SingleWinner(t *testing.T) {
    // 500 goroutines, same facility, same slot, released together
    // assert: exactly 1 confirmed, 499 clean 409s
    // assert: SELECT count(*) FROM bookings WHERE ... == 1
}
```

It runs against a real Postgres via testcontainers. A mock cannot exercise an exclusion constraint, which means a mocked version of this test proves nothing about the only thing being tested. Alongside it: idempotent replay returns the original booking and creates no second row; concurrent cancellations promote *distinct* waitlist entries (`SKIP LOCKED`, mutation-tested — plain `FOR UPDATE` also keeps them distinct, so there is a second test that specifically goes red on that mutation); a closure blocks bookings inside its window on both a court *and* the gym; and N concurrent requests against capacity C yield exactly C confirmations.

---

## Known limitations

Stated plainly, because a limitation you name is worth more than one a judge finds.

**The read replica has never been brought up in this environment.** `make dev-replica` is written but untested — it was cut for budget. `DB_REPLICA_URL` defaults to empty, which makes availability reads fall back to the primary. That fallback is a *supported* configuration, not a degraded one, and it is how everything in this repo is run and tested. The read/write split is real code on a real config flag; the standby behind it has not been exercised.

**`testutil.Slot18()` pins to *today* at 18:00 IST.** It is the contended slot every race test fights over, and it is computed from `time.Now()` rather than from a fixed future date — so any suite that uses it starts failing if the tests are run after 18:00 local time, because the booking is then in the past. Run the suites before 18:00 IST, or expect a batch of failures that have nothing to do with correctness.

**The full test suite is flaky at default parallelism.** `go test ./... -race` runs sixteen packages in parallel, each spinning its own Postgres container and some of them firing 500-goroutine races, which saturates a laptop Docker VM. Under that load the latency-budgeted tests in `test/booking` and `test/concurrency` intermittently fail — a few attempts hit `context deadline exceeded`, or the 40 ms alternatives budget expires and a 409 arrives bare. Every one of them passes in isolation and per-package, and no failure has ever implicated the invariant (`db_count = 1` held in all of them). **Run the full suite with `-p 4`.** This is ambient machine load, not a logic regression, and no test was weakened to hide it.

**Frontend status.** The Next.js client now lives in `/web` and covers the student discovery, booking, waitlist, claim, check-in, manager console, analytics, venue token and `/race` proof flows. Run `cd web && npm install && npm run dev`, then open `http://localhost:3000` while the API is running on `:8080`. `POST /api/v1/demo/race` remains the in-process proof backend used by the browser race console. Browser demo timing and full end-to-end rehearsal remain to be recorded.

---

## Scale, honestly

~8,000 students. Peak is 300–500 concurrent requests in the seconds after evening slots open. Reads beat writes roughly 100:1.

This is small. The bottleneck is contention on a single GiST index for one hot facility, which sustains write rates roughly two orders of magnitude above expected peak. When asked where it breaks, that is the answer — it is more convincing than premature infrastructure. The scale path (read replicas, monthly partitioning, per-facility sharding) is deliberately a plan rather than an implementation.
