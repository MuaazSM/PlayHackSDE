-- Unmet demand: waitlist depth by facility and hour-of-day. §10.2, FR-17.
--
-- This is the number utilisation cannot show. A court at 100% utilisation and a
-- court at 100% utilisation with eleven people queued behind it look identical
-- on the occupancy chart and mean completely different things to whoever
-- decides where the next court goes.
--
-- CANCELLED entries are excluded — somebody who left the queue is not unmet
-- demand. WAITING, PROMOTED, CLAIMED and EXPIRED all count: each one is a
-- student who wanted an hour that was already taken when they asked, which is
-- true regardless of how the offer later resolved.
--
-- Bucketed by the local hour the requested slot starts, for the same timezone
-- reason as the heatmap.
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
)
SELECT f.id                                                       AS facility_id,
       f.name                                                     AS facility_name,
       (EXTRACT(hour FROM lower(wl.during) AT TIME ZONE $3))::int  AS hour,
       count(*)::int                                              AS entries
  FROM waitlist wl
  JOIN facilities f ON f.id = wl.facility_id
  CROSS JOIN window_bounds w
 WHERE wl.status <> 'CANCELLED'
   AND lower(wl.during) <@ w.range
 GROUP BY f.id, f.name, 3
 ORDER BY f.name, 3;
