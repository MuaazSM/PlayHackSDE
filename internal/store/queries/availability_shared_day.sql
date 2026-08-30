-- Availability for one SHARED facility on one local day. §5.1, §3.3.
--
-- Same generated grid, but occupancy comes from the slot_capacity counter rather
-- than from row overlap: a shared facility has many simultaneous bookings by
-- design, so "does a booking overlap this slot" answers the wrong question.
--
-- A closure is still a BLOCKED booking row, and it outranks capacity — §10.4
-- also sets capacity = 0 for the closed slots, so the counter agrees, but
-- reading the row means a closure shows as closed even before that lands.
--
-- $1 facility id, $2 date (local), $3 timezone.
WITH f AS (
  SELECT * FROM facilities WHERE id = $1 AND is_active
),
grid AS (
  SELECT s AS slot_start,
         s + f.granularity AS slot_end,
         tstzrange(s, s + f.granularity, '[)') AS slot,
         f.capacity AS default_capacity
    FROM f,
         LATERAL generate_series(
           ($2::date + f.opens_at)  AT TIME ZONE $3,
           (($2::date + f.closes_at) AT TIME ZONE $3) - f.granularity,
           f.granularity
         ) AS s
)
SELECT g.slot_start,
       g.slot_end,
       CASE
         WHEN blk.status IS NOT NULL THEN 'closed'
         WHEN COALESCE(sc.capacity, g.default_capacity) - COALESCE(sc.booked, 0) <= 0
           THEN 'full'
         -- "filling" at 20% or less remaining: enough warning to hurry, not so
         -- much that the badge is always on.
         WHEN (COALESCE(sc.capacity, g.default_capacity) - COALESCE(sc.booked, 0))::numeric
              <= 0.2 * GREATEST(COALESCE(sc.capacity, g.default_capacity), 1)
           THEN 'filling'
         ELSE 'free'
       END AS state,
       COALESCE(sc.capacity, g.default_capacity) - COALESCE(sc.booked, 0) AS remaining,
       COALESCE(sc.capacity, g.default_capacity) AS capacity
  FROM grid g
  -- Only closures are read as rows here. Confirmed shared bookings are the
  -- counter's business.
  LEFT JOIN LATERAL (
    SELECT b.status
      FROM bookings b
     WHERE b.facility_id = $1
       AND b.status = 'BLOCKED'
       AND b.during && g.slot
     LIMIT 1
  ) blk ON true
  -- Counter rows are created lazily, so most slots have none. COALESCE to the
  -- facility's capacity is what makes an untouched slot read as fully free
  -- rather than as missing.
  LEFT JOIN slot_capacity sc
    ON sc.facility_id = $1
   AND sc.slot_start = g.slot_start
 ORDER BY g.slot_start;
