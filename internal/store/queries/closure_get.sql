-- Load one closure by id, for the reopen path's 404/409/convergence decision.
--
-- Separate from booking_get.sql because a closure's user_id is NULL and that
-- query's caller scans it into a non-nullable uuid. Restricting the projection
-- here also states what a closure is: a facility, a window and a status, with
-- nobody attached.
--
-- The reason lives in booking_events, not on the row — there is no reason column
-- and adding one would duplicate the audit trail. The creating event is the one
-- that moved the row to BLOCKED.
SELECT b.id,
       b.facility_id,
       f.name,
       b.is_exclusive,
       lower(b.during),
       upper(b.during),
       b.status::text,
       b.created_at,
       ev.reason
  FROM bookings b
  JOIN facilities f ON f.id = b.facility_id
  LEFT JOIN LATERAL (
    SELECT e.reason
      FROM booking_events e
     WHERE e.booking_id = b.id
       AND e.to_status = 'BLOCKED'
     ORDER BY e.at, e.id
     LIMIT 1
  ) ev ON true
 WHERE b.id = $1
   AND b.user_id IS NULL;
