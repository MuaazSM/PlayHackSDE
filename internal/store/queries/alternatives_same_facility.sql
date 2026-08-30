-- Alternatives, question 1: the SAME facility, later today. §5.3.
--
-- This is the fallback path. The default is an in-memory scan of the cached
-- campus grid (§5.2); this query only runs when that cache is cold, and it runs
-- inside a 40 ms budget on the REPLICA, after the write transaction has already
-- rolled back (§4.5). Nothing here may touch the primary or the aborted
-- transaction's connection.
--
-- A candidate is a start on the facility's own grid — generate_series steps from
-- opens_at, the same anchor validateAlignment uses, so a suggestion this query
-- makes is one the write path will accept. Candidates run to closes_at minus the
-- requested duration, so a suggestion never spills past closing time.
--
-- FREE IS DERIVED, exactly as it is on the read path. There is no is_available
-- column to consult (non-negotiable #4), so "free" is two NOT EXISTS clauses
-- against the same rows the write path inserts into:
--
--   * no overlapping exclusive booking or closure — Mechanism A's question;
--   * no full counter row inside the window   — Mechanism B's question.
--
-- Both are asked because this query does not know which mechanism the facility
-- uses, and asking both costs nothing: a shared facility has no exclusive rows
-- to match and an exclusive one has no counter rows.
--
-- These answers are ADVISORY. Between this SELECT and the user's next tap the
-- slot may be taken by someone else, and that is fine — the exclusion constraint
-- still decides, and the user gets another fast 409. This is a suggestion, never
-- a reservation, and it is emphatically not the read-then-write non-negotiable
-- #2 forbids: it runs AFTER a failed write, never before one.
--
-- $1 facility id, $2 date (local), $3 timezone, $4 requested duration,
-- $5 earliest acceptable start, $6 row limit.
WITH f AS (
  SELECT * FROM facilities WHERE id = $1 AND is_active
),
cand AS (
  SELECT s AS slot_start,
         s + $4::interval AS slot_end
    FROM f,
         LATERAL generate_series(
           ($2::date + f.opens_at)  AT TIME ZONE $3,
           (($2::date + f.closes_at) AT TIME ZONE $3) - $4::interval,
           f.granularity
         ) AS s
   WHERE s > $5::timestamptz
)
SELECT f.id, f.name, f.sport, c.slot_start, c.slot_end
  FROM cand c, f
 -- Never suggest a window the facility would refuse on duration grounds.
 WHERE $4::interval BETWEEN f.min_duration AND f.max_duration
   AND NOT EXISTS (
         SELECT 1
           FROM bookings b
          WHERE b.facility_id = f.id
            AND (b.is_exclusive OR b.status = 'BLOCKED')
            AND b.status IN ('CONFIRMED','HELD','BLOCKED')
            AND b.during && tstzrange(c.slot_start, c.slot_end, '[)')
       )
   AND NOT EXISTS (
         SELECT 1
           FROM slot_capacity sc
          WHERE sc.facility_id = f.id
            AND sc.slot_start >= c.slot_start
            AND sc.slot_start <  c.slot_end
            AND sc.booked >= sc.capacity
       )
 -- Nearest first: the next free hour is worth more than one at closing time.
 ORDER BY c.slot_start
 LIMIT $6;
