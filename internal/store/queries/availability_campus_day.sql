-- The whole campus for one day, in ONE query. §5.2, FR-02, G-1.
--
-- One request per date is a hard requirement, not an optimisation. The discovery
-- screen shows every facility at once; issuing a query per facility would make
-- the page cost scale with the catalogue and turn a single slow read into seven.
--
-- Both mechanisms are resolved in the same pass: exclusive facilities from row
-- overlap, shared ones from the slot_capacity counter. The CASE below is the
-- only place those two answers are unified, and it reads top to bottom as the
-- precedence a student cares about — outside hours, then closed, then taken,
-- then how full.
--
-- $1 date (local), $2 timezone.
WITH params AS (
  SELECT $1::date AS day, $2::text AS tz
),
active AS (
  SELECT * FROM facilities WHERE is_active
),
-- A single shared time axis, so the result is a dense rectangle the client can
-- render as a table. It spans the union of every facility's opening hours;
-- cells outside a given facility's own hours are marked closed below.
bounds AS (
  SELECT min(opens_at)    AS opens,
         max(closes_at)   AS closes,
         min(granularity) AS step
    FROM active
),
grid AS (
  SELECT s AS slot_start, s + b.step AS slot_end
    FROM params p, bounds b,
         LATERAL generate_series(
           (p.day + b.opens)  AT TIME ZONE p.tz,
           ((p.day + b.closes) AT TIME ZONE p.tz) - b.step,
           b.step
         ) AS s
),
-- The day's outer bounds, so the slot_capacity join can be restricted to this
-- day's counter rows. Without it the planner hashes the WHOLE table: correct,
-- and fine at a few thousand rows, but slot_capacity grows by one row per shared
-- slot per day forever, so an unbounded scan here is the first thing that
-- degrades as the term wears on.
span AS (
  SELECT min(slot_start) AS lo, max(slot_end) AS hi FROM grid
),
cell AS (
  SELECT f.id AS facility_id, f.name, f.sport, f.is_exclusive, f.capacity,
         g.slot_start, g.slot_end,
         tstzrange(g.slot_start, g.slot_end, '[)') AS slot,
         (p.day + f.opens_at)  AT TIME ZONE p.tz AS facility_opens,
         (p.day + f.closes_at) AT TIME ZONE p.tz AS facility_closes
    FROM active f
   CROSS JOIN grid g
   CROSS JOIN params p
)
SELECT c.facility_id, c.name, c.sport, c.is_exclusive,
       c.slot_start, c.slot_end,
       CASE
         -- A facility that is not open yet is not free.
         WHEN c.slot_start < c.facility_opens OR c.slot_end > c.facility_closes
           THEN 'closed'
         WHEN b.status = 'BLOCKED'   THEN 'closed'
         WHEN c.is_exclusive AND b.status = 'CONFIRMED' THEN 'booked'
         WHEN c.is_exclusive AND b.status = 'HELD'      THEN 'held'
         WHEN c.is_exclusive                            THEN 'free'
         WHEN COALESCE(sc.capacity, c.capacity) - COALESCE(sc.booked, 0) <= 0
           THEN 'full'
         WHEN (COALESCE(sc.capacity, c.capacity) - COALESCE(sc.booked, 0))::numeric
              <= 0.2 * GREATEST(COALESCE(sc.capacity, c.capacity), 1)
           THEN 'filling'
         ELSE 'free'
       END AS state
  FROM cell c
  -- Exclusive rows answer "is this slot taken"; for shared facilities only a
  -- closure is a row-level fact, so the predicate keeps the scan off the thirty
  -- confirmed gym bookings that the counter already accounts for.
  --
  -- As in the per-facility query, the ORDER BY cannot change the answer today:
  -- the exclusion constraint admits one overlapping row per exclusive facility,
  -- and for a shared one only BLOCKED rows match here and all map to 'closed'.
  --
  -- Reachable only if no_double_book's predicate NARROWS below the statuses read
  -- here, or if this filter widens past it. Until then it is a statement of
  -- intent, not working logic.
  LEFT JOIN LATERAL (
    SELECT b.status
      FROM bookings b
     WHERE b.facility_id = c.facility_id
       AND (b.is_exclusive OR b.status = 'BLOCKED')
       AND b.status IN ('CONFIRMED','HELD','BLOCKED')
       AND b.during && c.slot
     ORDER BY CASE b.status
                WHEN 'BLOCKED'   THEN 0
                WHEN 'CONFIRMED' THEN 1
                ELSE 2
              END
     LIMIT 1
  ) b ON true
  -- The range predicates are redundant with the equality above, and they are
  -- what keeps this join bounded to one day.
  --
  -- Measured (EXPLAIN ANALYZE, plans below): without them the planner hashes
  -- EVERY counter row — and it does not change strategy as the table grows, so
  -- this is not something to leave to the optimiser. At an artificial 214k rows
  -- that was 11.5ms of an 81ms query; with them the same scan returns 6 rows.
  --
  -- Written as scalar subqueries so they become InitPlan constants. A correlated
  -- reference to a CTE column cannot be pushed into the scan and left the full
  -- scan in place.
  --
  -- It is still a sequential scan, not an index scan: slot_capacity's primary
  -- key is (facility_id, slot_start), so a slot_start-only range is not a usable
  -- prefix. An index on slot_start alone would fix that and is NOT worth adding
  -- — the gym writes 18 counter rows a day, so a year is under 7k rows and the
  -- filtered scan costs 1.3ms. Revisit only if shared facilities multiply.
  --
  -- Safe in the ON clause of a LEFT JOIN: this narrows the right side, it never
  -- drops a left row.
  LEFT JOIN slot_capacity sc
    ON sc.facility_id = c.facility_id
   AND sc.slot_start  = c.slot_start
   AND sc.slot_start >= (SELECT lo FROM span)
   AND sc.slot_start  < (SELECT hi FROM span)
 ORDER BY c.sport, c.name, c.slot_start;

-- ---------------------------------------------------------------------------
-- EXPLAIN ANALYZE, 10,120 bookings and 5,580 counter rows (~310 days of gym
-- operation), postgres:16-alpine.
--
--  Sort (actual time=44.7..44.8 rows=126)
--    -> Nested Loop Left Join (rows=126)
--       -> Hash Left Join (rows=126)
--          -> Nested Loop (rows=126)
--             -> CTE Scan on active f (rows=7)
--             -> Materialize (rows=18, loops=7)
--          -> Seq Scan on slot_capacity sc (actual time=0.025..1.264 rows=13)
--             Filter: slot_start >= InitPlan AND slot_start < InitPlan
--       -> Limit (loops=126)
--          -> Bitmap Heap Scan on bookings b (rows=1, loops=126)
--             -> Bitmap Index Scan on idx_bookings_day (0.008ms, loops=126)
--                Index Cond: facility_id = ... AND during && tstzrange(...)
--  Execution Time: 44.798 ms
--
-- What matters in that plan:
--   * bookings is reached through idx_bookings_day, the GiST index, on every
--     one of the 126 cells. There is NO sequential scan on bookings.
--   * The only sequential scans are facilities (7 rows — the planner is right
--     to ignore an index there) and the day-bounded slot_capacity filter.
--   * 126 cells = 7 facilities x 18 slots, one query, no N+1.
-- ---------------------------------------------------------------------------
