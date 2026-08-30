-- Availability for one EXCLUSIVE facility on one local day. §5.1.
--
-- Availability is DERIVED, every time, from the bookings table. There is no
-- is_available column and there never will be (non-negotiable #4): two fields
-- that could disagree eventually will, and the one that disagrees silently is
-- the one users act on.
--
-- The grid is generated, not stored. No nightly job materialises slots, so a
-- facility whose hours change is correct on the next request rather than after
-- the next batch run.
--
-- $1 facility id, $2 date (local), $3 timezone (TZ_DISPLAY — never hardcoded;
-- the server may run anywhere and the student is always in IST).
WITH f AS (
  SELECT * FROM facilities WHERE id = $1 AND is_active
),
grid AS (
  SELECT s AS slot_start,
         s + f.granularity AS slot_end,
         tstzrange(s, s + f.granularity, '[)') AS slot
    FROM f,
         LATERAL generate_series(
           ($2::date + f.opens_at)  AT TIME ZONE $3,
           (($2::date + f.closes_at) AT TIME ZONE $3) - f.granularity,
           f.granularity
         ) AS s
)
SELECT g.slot_start,
       g.slot_end,
       CASE b.status
         WHEN 'BLOCKED'   THEN 'closed'
         WHEN 'HELD'      THEN 'held'
         WHEN 'CONFIRMED' THEN 'booked'
         ELSE 'free'
       END AS state
  FROM grid g
  -- LATERAL + LIMIT 1 so a slot resolves to one state instead of duplicating.
  --
  -- The ORDER BY is defensive, not load-bearing, and it is worth being honest
  -- about which: no_double_book already guarantees that at most ONE row among
  -- CONFIRMED/HELD/BLOCKED overlaps any instant on an exclusive facility, so
  -- there is never a second row to outrank. It stays because it costs nothing
  -- and states the intended precedence if that predicate is ever widened —
  -- a closure outranks a booking, which outranks a hold.
  LEFT JOIN LATERAL (
    SELECT b.status
      FROM bookings b
     WHERE b.facility_id = $1
       AND b.is_exclusive
       AND b.status IN ('CONFIRMED','HELD','BLOCKED')
       AND b.during && g.slot
     ORDER BY CASE b.status
                WHEN 'BLOCKED'   THEN 0
                WHEN 'CONFIRMED' THEN 1
                ELSE 2
              END
     LIMIT 1
  ) b ON true
 ORDER BY g.slot_start;

-- ---------------------------------------------------------------------------
-- EXPLAIN ANALYZE, 10,120 bookings, postgres:16-alpine.
--
--  Sort (actual time=0.927..0.929 rows=16) Buffers: shared hit=79
--    -> Nested Loop Left Join (rows=16)
--       -> Nested Loop (rows=16)
--          -> Seq Scan on facilities (rows=1, 7 scanned)
--          -> Function Scan on generate_series s (rows=16)
--       -> Limit (loops=16)
--          -> Bitmap Heap Scan on bookings b (rows=1, loops=16)
--             Recheck Cond: facility_id = ... AND during && tstzrange(...)
--             -> Bitmap Index Scan on idx_bookings_day (0.016ms, loops=16)
--
-- The GiST index serves every slot lookup; there is no sequential scan on
-- bookings. facilities is scanned because it holds seven rows and an index
-- would cost more than reading them.
--
-- Sub-millisecond for a facility-day, entirely from shared buffers.
-- ---------------------------------------------------------------------------
