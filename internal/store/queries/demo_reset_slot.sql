-- Clear the demo slot so the race is re-runnable, live, on stage, twice.
-- IMPLEMENTATION.md §13.
--
-- Cancels every live booking overlapping the window at one facility. HELD rows
-- are included because they sit in no_double_book's predicate too: leaving one
-- behind would make the next run produce zero winners, which looks exactly like
-- a broken demo.
--
-- There is NO "mark the slot free" step, and its absence is non-negotiable #4.
-- Moving a row to CANCELLED drops it out of the constraint's partial index and
-- the slot is bookable again the instant this transaction commits. Availability
-- is derived from these rows at read time, so there is no second field to clear
-- and nothing that could disagree.
--
-- This is NOT booking.Cancel and must not grow into it. A student cancelling
-- their court is a real event: it promotes the head of the waitlist and notifies
-- people. A demo reset is stage management — it must not page anybody, and it
-- must not hand the slot to a waitlisted student half a second before the
-- presenter fires the race again. The booking_events rows the caller writes
-- alongside this keep the audit trail honest about what happened.
--
-- The prev CTE reads the pre-UPDATE snapshot, so from_status is the status each
-- booking actually had rather than the CANCELLED that RETURNING would report.
WITH prev AS (
  SELECT id, status AS from_status
    FROM bookings
   WHERE facility_id = $1
     AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
     AND status IN ('CONFIRMED', 'HELD')
),
upd AS (
  UPDATE bookings
     SET status = 'CANCELLED'
   WHERE facility_id = $1
     AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
     AND status IN ('CONFIRMED', 'HELD')
  RETURNING id, is_exclusive, lower(during) AS starts, upper(during) AS ends
)
SELECT upd.id, upd.is_exclusive, upd.starts, upd.ends, prev.from_status::text
  FROM upd
  JOIN prev ON prev.id = upd.id;
