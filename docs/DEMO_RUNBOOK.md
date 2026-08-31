# Demo runbook

Six beats, 3 min 20 s. Source: IMPLEMENTATION.md §17.

The thing being demonstrated is that a database constraint decides the winner of a race. Every beat either sets that up or proves it. If time is lost, lose it from beats 1, 2 and 5 — **never** from beat 3.

Read the Known limitations section of the README before rehearsing. The web client now covers beats 2 and 4; `testutil.Slot18()` still pins the contended slot to *today at 18:00 IST*, so a demo given after 18:00 local time needs `-at` moved forward (see the beat 3 fallback).

---

## Pre-flight

Run this **30 minutes before** the slot, not five. Every step below has failed for somebody at least once.

### Environment

- [ ] **Laptop off venue wifi entirely.** Not "on a different network" — off. Nothing in the demo needs the internet, and a captive portal that hijacks DNS is a failure mode you cannot debug on stage.
- [ ] Docker Desktop running, and given enough memory. The concurrency suites spin per-package Postgres containers; a 7.8 GB VM is where the parallelism flake was measured.
- [ ] Check for a host-installed PostgreSQL or Redis: `lsof -nP -iTCP:5432 -sTCP:LISTEN` and `lsof -nP -iTCP:6379 -sTCP:LISTEN`. If either is occupied, use the host-port overrides on **every** make target for the whole demo:
      `POSTGRES_HOST_PORT=55432 PGBOUNCER_HOST_PORT=56432 REDIS_HOST_PORT=56379 make dev`
- [ ] Screen recording of a successful full run sitting on the desktop, ready to play. This is the fallback of last resort and it has to exist before you need it.

### Stack

- [ ] `make dev` — waits for postgres + redis health and prints `docker compose ps`. Confirm every service is `healthy` or `running` before moving on.
- [ ] `make migrate-up` — applies 0001–0010. Note this goes **direct to Postgres**, not through PgBouncer, because `golang-migrate` takes a session advisory lock a transaction-mode pooler cannot honour. If it hangs, you are pointed at 6432.
- [ ] `make seed` — 7 facilities (6 exclusive courts + the gymnasium at capacity 30), 12 users (`student01`..`student10`, `manager01`, `secretary01`), 1 global policy. Idempotent, so re-run it freely.
- [ ] `make psql` then `\dx` — confirm `btree_gist` is installed. This is the single dependency the whole design rests on. Verify it, do not assume it.
- [ ] `make run` — API on :8080. Leave it running in its own terminal, visible if the setup allows.
- [ ] `curl -s localhost:8080/readyz` returns `{"status":"ready","redis":"ok"}`. If `redis` reads `degraded` the demo still works — Redis is never authoritative — but say so before a judge notices.

### Configuration

- [ ] `AUTH_MODE=dev` (the default). Without it, `POST /api/v1/dev/login`, `POST /api/v1/demo/race` and `POST /api/v1/demo/reset` **are not registered at all** — they do not 403, they 404, because an endpoint that mints tokens for any roll number should not be one config flag away from serving.
- [ ] `NOTIFIER=log` (the default). `webpush` without VAPID keys is a boot error by design.
- [ ] `CHECKIN_HMAC_SECRET` set if you intend to show check-in. Empty means every check-in is refused and the API warns about it at boot — scroll back and look for that line.
- [ ] `WRITE_QUEUE_DEPTH` left at the application and Makefile default of 24. At 128 this class of hardware misses the 150 ms rejection budget (measured 368 ms).

### Rehearsal

- [ ] `make race-demo N=500` — fire it **twice**, back to back. Expect `confirmed=1 conflicts=499 other=0 db_count=1` both times. The target resets the slot by default, which is what makes it re-runnable on stage.
- [ ] `make race-demo N=500 RACE_ARGS=-reset=false` once — this is the "fire again — still 1" beat. Expect `confirmed=0 conflicts=500 db_count=1`.
- [ ] `make audit-live` once, straight after a race, so you know the invariant suite is green on this machine. It reads only, so it is safe to run again on stage if a judge asks how you know the data is consistent — and it is far better evidence than a rebuilt scenario, because it audits the database they just watched you hammer.
- [ ] `make race-reset` — leaves the slot clean for the real run.
- [ ] Optional, and worth the two minutes: `make load N=500`. It is a hard pass/fail gate (non-zero exit if p99(409) ≥ 150 ms, p99(201) ≥ 250 ms, or any 5xx). If you are going to claim the latency numbers on stage, have a green run from *this* laptop, *today*.
- [ ] Second screen logged in as the waitlisted student. Grab a token now: `curl -s -XPOST localhost:8080/api/v1/dev/login -d '{"roll_no":"student05"}'` and keep it in a scratch file — do not type a JWT on stage.
- [ ] Grafana loaded on browser tab 2: `http://localhost:3001` (anonymous access is on; it opens straight on the surge board). Eyeball the panels — the DB-count panel queries Postgres directly and only works from inside the compose network.
- [ ] Terminal font size raised until the back row can read `db_count=1`. This is not cosmetic; that number is the entire proof.

---

## The six beats

### 1. Problem — 20 s

**Say:** "Six PM. Evening slots open. Four hundred students hit refresh on the same tennis court in the same three seconds. Today that ends in a WhatsApp argument and a double-booked court, because the system checked availability and then wrote — and between those two steps, somebody else wrote too. That gap is the entire problem. Not scale. Eight thousand students is nothing. It is correctness under contention."

**Do:** Nothing. Slide one, or just talk. Keep your hands off the keyboard so the room watches you and not a terminal.

**If it fails:** There is nothing to fail. If you are already behind schedule when you start, cut this to one sentence — "hundreds of students, one court, one winner" — and move.

---

### 2. Product — 60 s

**Say:** "Before the proof, the product. A student opens the campus grid — every facility, every hour, one request. Picks 6 PM on Tennis Court 1. Confirms. One tap, an explicit confirmation with a reference number, and the availability everyone else is looking at updates over SSE without a refresh." Deliberately unhurried — this is the usability criterion and rushing it reads as having nothing to show.

**Do:** Open the web client at `http://localhost:3000`, sign in as `student01`, select the campus date and tap a free cell in the discovery grid. The confirmation view shows the booking reference and check-in reminder. Keep the three prepared curls below as a deterministic fallback if the browser or API is unavailable:

The API takes a **UUID** for `facility_id` and an **RFC3339 timestamp** for `start` — only the `racedemo` CLI accepts a slug and `HH:MM`. Resolve the id once and keep it in a variable:

```bash
TOKEN=$(curl -s -XPOST localhost:8080/api/v1/dev/login \
  -H 'Content-Type: application/json' \
  -d '{"roll_no":"student01"}' | jq -r .token)

FID=$(curl -s localhost:8080/api/v1/facilities -H "Authorization: Bearer $TOKEN" \
  | jq -r '.facilities[] | select(.name=="Tennis Court 1") | .id')

START="$(date +%F)T18:00:00+05:30"
KEY=$(uuidgen)

curl -s "localhost:8080/api/v1/availability?date=$(date +%F)" \
  -H "Authorization: Bearer $TOKEN" | jq

curl -s -XPOST localhost:8080/api/v1/bookings \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"facility_id\":\"$FID\",\"start\":\"$START\",\"duration_minutes\":60}" | jq
```

Then re-run that last command **unchanged** — same `$KEY` — and show it returns `200` with the original booking rather than `201` with a second one. Same intention, same key, one row.

**If it fails:** If a curl errors, do not debug on stage. Say "the write path is what beat three proves — let me show you that" and go straight to beat 3. Beat 2 is the most cuttable minute in the runbook.

---

### 3. Proof — 45 s — **the beat that matters**

**Say:** "Five hundred requests. One court. One 6 PM slot. All released on a closed channel so they genuinely contend rather than trickle." Fire. Then, pointing at the last line: "One confirmed. Four hundred and ninety-nine rejected. And this number — `db_count` — is not a counter we kept. It is a fresh `SELECT count(*)` against the bookings table, issued after every goroutine finished. One row. Now watch me do it again without resetting."

**Do:**

```bash
make race-demo N=500
```

It prints a readable split — elapsed, p50/p99, start spread (how simultaneous the goroutines really were), the literal `SELECT count(*)` it issued, and then the summary line:

```
confirmed=1 conflicts=499 other=0 db_count=1
```

Then, the "fire again — still 1" move:

```bash
make race-demo N=500 RACE_ARGS=-reset=false
```

Expect `confirmed=0 conflicts=500 other=0 db_count=1` — the slot is already taken, so nobody wins and the count has not moved.

Do not read the reject p99 from this output as the M-3 number. The demo path deliberately runs without the shedder, so all N are admitted and the rejection curve is the unshed one (~300–420 ms) — the output labels it as such. The `<150 ms` claim is the shed path, and `make load` is what measures it.

To clean up between takes:

```bash
make race-reset
```

`make race-demo` resets the slot before firing by default, which is what makes it re-runnable back to back on stage; `-reset=false` is the only way to get the second reading. The whole thing runs in-process against the local database — no HTTP, no rate limiter, no shedder distorting the picture, and no network. Venue wifi cannot break this beat.

**If it fails:** Three fallbacks in order. (1) If it errors with a slot-in-the-past problem, it is after 18:00 local — the demo slot is *today* at 18:00 IST. Move it: `make race-demo N=500 RACE_ARGS="-at=21:00"`. (2) If the database is unreachable, `make down && make dev && make migrate-up && make seed` takes about ninety seconds and you should narrate beat 5 while it runs. (3) If it still will not go, play the screen recording and say plainly that you are showing a recording. Never fake the number.

---

### 4. Recovery — 30 s

**Say:** "Losing cleanly is half the answer. The other half is what happens next. Student five is on the waitlist. I cancel the winner — and the cancel transaction claims the head of the queue with `FOR UPDATE SKIP LOCKED`, inserts a held reservation, and enqueues the notification inside the same transaction through an outbox, so nobody is ever told about a booking that did not commit. `SKIP LOCKED` is why two simultaneous cancellations promote two *different* students instead of fighting over one."

**Do:** With the waitlisted student's token already in a variable, join the queue, then cancel the winner from the first terminal and show the promotion landing:

```bash
curl -s -XPOST localhost:8080/api/v1/waitlist \
  -H "Authorization: Bearer $TOKEN5" -H 'Content-Type: application/json' \
  -d "{\"facility_id\":\"$FID\",\"start\":\"$START\",\"duration_minutes\":60}" | jq

curl -s -XDELETE "localhost:8080/api/v1/bookings/$WINNER_ID" \
  -H "Authorization: Bearer $TOKEN"
```

Reuse `$FID` and `$START` from beat 2. The waitlist is **exclusive facilities only** — a held row reserves nothing on a counter-based gym, so `POST /api/v1/waitlist` returns `422` for the gymnasium by design rather than promising an offer it cannot keep.

Second screen: either an open SSE stream (`curl -N "localhost:8080/api/v1/stream?date=$(date +%F)&access_token=$TOKEN5"`) or the API log showing `waitlist.promoted` drained from the outbox. Then `POST /api/v1/bookings/{id}/claim` as student05 to turn the hold into a confirmation.

**If it fails:** If the promotion does not appear within a couple of seconds, say "the outbox dispatcher falls back to a five-second ticker when the `LISTEN` connection is not direct to Postgres — that is a wiring detail, the transaction already committed" and show `SELECT * FROM outbox ORDER BY id DESC LIMIT 5` in `make psql` instead. The row is the proof; the notification is the delivery.

---

### 5. Mechanism — 30 s

**Say:** "One slide. This is the whole system." Read the constraint aloud, then: "The handler does a plain `INSERT`. No `SELECT` first — that gap is the bug. Postgres serialises the conflicting inserts on the GiST index and raises `23P01` for every loser, which we map to a 409 with alternatives. We did not use `SELECT FOR UPDATE`, because with no existing row there is nothing to lock and two inserts into an empty slot both succeed. We did not use a version column, because that needs a retry loop and isolation-level reasoning for a weaker guarantee. We did not use a Redis lock, because that is a second source of truth and divergence kills the invariant silently. There is a per-facility advisory lock in the transaction, and I will be precise about it: it is a contention shaper, not the concurrency control. Without it, five hundred concurrent inserts deadlock *inside* the constraint check — we measured about a hundred and seventy deadlock victims. With the constraint's predicate disabled the race test fails at five hundred confirmations. That is the mutation proof: the constraint decides, the lock only smooths."

**Do:** One slide:

```sql
CONSTRAINT no_double_book
  EXCLUDE USING gist (facility_id WITH =, during WITH &&)
  WHERE (is_exclusive AND status IN ('CONFIRMED','HELD','BLOCKED'))
```

Add one line for the gym: `UPDATE slot_capacity SET booked = booked + 1 WHERE ... AND booked < capacity`. Different problem, different tool, same transaction.

**If it fails:** A slide deck that will not open is not an emergency. `make psql`, then paste two overlapping `INSERT`s and let the second one raise `23P01` in front of the room. That is a better demo than the slide anyway — keep the two statements in a scratch file so you can do it deliberately.

---

### 6. Path — 15 s

**Say:** "Where does this break? One GiST index on one hot facility, roughly a hundred times above our actual peak. Eight thousand students, a few hundred bookings a day — this is a small system with a hard correctness requirement, and we built for the requirement, not for imagined scale. The scale path is read replicas, monthly partitioning, per-facility sharding. That is a slide, not a sprint. Thank you."

**Do:** Stop talking. Do not open another terminal.

**If it fails:** Nothing to fail. If you are over time, cut to: "It breaks at about a hundred times our peak, on one index, and we know exactly where. Thank you."

---

## Recovery cheat sheet

Keep this visible while presenting.

| Symptom | Command |
|---|---|
| Demo slot dirty between takes | `make race-reset` |
| "Fire again, still 1" | `make race-demo N=500 RACE_ARGS=-reset=false` |
| Slot is in the past (after 18:00 IST) | `make race-demo N=500 RACE_ARGS="-at=21:00"` |
| Database wedged | `make down && make dev && make migrate-up && make seed` |
| Need to show the raw table | `make psql`, then `SELECT id, user_id, during, status FROM bookings WHERE status='CONFIRMED' ORDER BY created_at DESC LIMIT 5;` |
| Demo routes 404 | `AUTH_MODE` is not `dev`; restart `make run` with it set |
| Port collision with a host Postgres/Redis | Prefix every target with `POSTGRES_HOST_PORT=55432 PGBOUNCER_HOST_PORT=56432 REDIS_HOST_PORT=56379` |
| Latency claim challenged | `make load N=500` — hard gate, exits non-zero if the budgets are missed |
| "How do we know the data is consistent?" | `make audit-live` — read-only invariant checks against the database you just hammered |
