-- No-show rate by facility. §10.2, FR-17; feeds M-6.
--
-- Denominator is every booking that REACHED its slot: CONFIRMED, COMPLETED and
-- NO_SHOW. A cancellation is not a no-show — the student gave the slot back in
-- time for the waitlist to take it, which is the behaviour the system wants —
-- so counting cancellations in the denominator would reward cancelling by
-- diluting the rate, and counting them in the numerator would punish it.
--
-- BLOCKED rows are closures with no user; they cannot show up or fail to.
--
-- Every active facility appears, including ones with no bookings at all, so an
-- empty range yields zeroes rather than a missing row the client must guess at.
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
scoped AS (
  SELECT b.facility_id, b.status
    FROM bookings b, window_bounds w
   WHERE b.status IN ('CONFIRMED', 'COMPLETED', 'NO_SHOW')
     AND lower(b.during) <@ w.range
)
SELECT f.id                                                        AS facility_id,
       f.name                                                      AS facility_name,
       COALESCE(count(s.status), 0)::int                           AS total,
       COALESCE(count(*) FILTER (WHERE s.status = 'NO_SHOW'), 0)::int AS no_shows,
       CASE WHEN count(s.status) = 0 THEN 0::float8
            ELSE count(*) FILTER (WHERE s.status = 'NO_SHOW')::float8
                 / count(s.status)::float8
       END AS rate
  FROM facilities f
  LEFT JOIN scoped s ON s.facility_id = f.id
 WHERE f.is_active
 GROUP BY f.id, f.name
 ORDER BY f.name;
