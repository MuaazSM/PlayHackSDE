-- Slot recovery: promoted waitlist entries that became attended bookings.
-- §10.2, FR-17; feeds M-7.
--
-- This is the payoff metric for the whole waitlist mechanism. A cancellation
-- that promotes somebody who then never turns up recovered nothing; the slot
-- was wasted twice. So the numerator is deliberately ATTENDANCE (a check_ins
-- row, or a booking the sweeper closed out as COMPLETED), not promotion and not
-- even claiming.
--
-- Denominator is every entry that was actually offered a slot — status
-- PROMOTED, CLAIMED or EXPIRED with a booking attached. Entries still WAITING
-- were never offered anything and cannot have failed to take it.
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
offered AS (
  SELECT wl.id,
         wl.booking_id
    FROM waitlist wl, window_bounds w
   WHERE wl.status IN ('PROMOTED', 'CLAIMED', 'EXPIRED')
     AND wl.booking_id IS NOT NULL
     AND lower(wl.during) <@ w.range
)
SELECT count(*)::int AS promoted,
       count(*) FILTER (
         WHERE EXISTS (SELECT 1 FROM check_ins ci WHERE ci.booking_id = o.booking_id)
            OR EXISTS (SELECT 1 FROM bookings b
                        WHERE b.id = o.booking_id AND b.status = 'COMPLETED')
       )::int AS recovered,
       CASE WHEN count(*) = 0 THEN 0::float8
            ELSE count(*) FILTER (
                   WHERE EXISTS (SELECT 1 FROM check_ins ci WHERE ci.booking_id = o.booking_id)
                      OR EXISTS (SELECT 1 FROM bookings b
                                  WHERE b.id = o.booking_id AND b.status = 'COMPLETED')
                 )::float8 / count(*)::float8
       END AS rate
  FROM offered o;
