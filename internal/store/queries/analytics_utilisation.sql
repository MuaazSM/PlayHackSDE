-- Utilisation by facility and hour-of-day. §10.2, FR-17.
--
-- DERIVED, like availability (non-negotiable #4). There is no rollup table and
-- no counter maintained on the write path: at a few hundred bookings a day the
-- honest query is trivially fast, and a number that is recomputed cannot drift
-- from the rows it claims to summarise. A summary table would be a second
-- source of truth about occupancy, which is the same mistake as is_available
-- wearing a reporting hat.
--
-- available_hours is the OPEN capacity of the cell: one hour per day the
-- facility is open at that hour, multiplied by capacity. For an exclusive court
-- capacity is 1, so a day's 18:00 cell offers one hour. For the gymnasium
-- (capacity 30) it offers thirty person-hours, which is the only denominator
-- under which a half-full gym reads as 50% rather than 1500%.
--
-- booked_hours counts CONFIRMED, COMPLETED and NO_SHOW. A no-show still held
-- the court — it was unavailable to everyone else — so excluding it would
-- report the facility as idle during the exact hour the no-show report is
-- about. CANCELLED never occupied anything; BLOCKED is a closure, which removes
-- supply rather than consuming it, and is excluded from both sides.
--
-- $1 from date (local, inclusive), $2 to date (local, inclusive), $3 timezone.
WITH days AS (
  SELECT d::date AS d
    FROM generate_series($1::date, $2::date, '1 day') AS d
),
hours AS (
  SELECT h FROM generate_series(0, 23) AS h
),
cells AS (
  SELECT f.id       AS facility_id,
         f.name     AS facility_name,
         f.capacity AS capacity,
         hr.h       AS hour,
         tstzrange(
           (dy.d + make_interval(hours => hr.h))     AT TIME ZONE $3,
           (dy.d + make_interval(hours => hr.h + 1)) AT TIME ZONE $3,
           '[)'
         ) AS win
    FROM facilities f
    CROSS JOIN days dy
    CROSS JOIN hours hr
   WHERE f.is_active
     -- whole-hour cells only inside opening hours; a cell that straddles the
     -- close is not supply the facility ever offered
     AND make_interval(hours => hr.h)     >= f.opens_at
     AND make_interval(hours => hr.h + 1) <= f.closes_at
),
occupied AS (
  SELECT c.facility_id,
         c.facility_name,
         c.capacity,
         c.hour,
         COALESCE((
           SELECT sum(
                    EXTRACT(epoch FROM (upper(b.during * c.win) - lower(b.during * c.win)))
                  ) / 3600.0
             FROM bookings b
            WHERE b.facility_id = c.facility_id
              AND b.status IN ('CONFIRMED', 'COMPLETED', 'NO_SHOW')
              AND b.during && c.win
         ), 0)::float8 AS booked_hours
    FROM cells c
)
SELECT facility_id,
       facility_name,
       hour,
       (count(*) * capacity)::float8 AS available_hours,
       sum(booked_hours)::float8     AS booked_hours,
       CASE WHEN count(*) * capacity = 0 THEN 0::float8
            ELSE sum(booked_hours) / (count(*) * capacity)
       END AS utilisation
  FROM occupied
 GROUP BY facility_id, facility_name, hour, capacity
 ORDER BY facility_name, hour;
