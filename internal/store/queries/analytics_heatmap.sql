-- Peak-demand heatmap: day-of-week x hour-of-day. §10.2, FR-17.
--
-- DEMAND, not occupancy. A booking and a waitlist entry are both somebody who
-- wanted that hour, and counting only the bookings would report the most
-- oversubscribed cell on campus as merely "full" — the same number a quiet
-- single-booking hour produces. Utilisation saturates at 1.0; this does not,
-- which is what makes it a peak finder.
--
-- The bucket is the hour the requested slot STARTS, resolved in the campus
-- timezone ($3). Bucketing in UTC would smear IST's 18:00 rush across two
-- columns and put a late-evening slot on the previous day.
--
-- isodow: 1 = Monday .. 7 = Sunday. The caller subtracts one to index a dense
-- 7x24 matrix; missing cells are zeroes there, not absent rows.
--
-- $1 from date (local, inclusive), $2 to date (local, inclusive), $3 timezone.
WITH window_bounds AS (
  -- ($1::date)::timestamp, not ($1::date). `date AT TIME ZONE zone` casts the
  -- date to timestamptz FIRST, using the session zone, and then converts it —
  -- which shifts the boundary the wrong way and quietly drops the campus
  -- morning out of every report. Casting to timestamp first means "this wall
  -- clock, in that zone", which is the question actually being asked.
  SELECT tstzrange(
           ($1::date)::timestamp       AT TIME ZONE $3,
           ($2::date + 1)::timestamp   AT TIME ZONE $3,
           '[)'
         ) AS range
),
demand AS (
  SELECT lower(b.during) AS at
    FROM bookings b, window_bounds w
   WHERE b.status IN ('CONFIRMED', 'COMPLETED', 'NO_SHOW', 'HELD')
     AND lower(b.during) <@ w.range

  UNION ALL

  SELECT lower(wl.during) AS at
    FROM waitlist wl, window_bounds w
   WHERE wl.status <> 'CANCELLED'
     AND lower(wl.during) <@ w.range
)
SELECT (EXTRACT(isodow FROM at AT TIME ZONE $3))::int AS isodow,
       (EXTRACT(hour   FROM at AT TIME ZONE $3))::int AS hour,
       count(*)::int                                  AS demand
  FROM demand
 GROUP BY 1, 2
 ORDER BY 1, 2;
