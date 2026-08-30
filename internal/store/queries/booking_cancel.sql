-- Cancel, guarded by status. IMPLEMENTATION.md §6.1.
--
-- The WHERE status IN ('CONFIRMED','HELD') guard is the concurrency control: two
-- simultaneous cancels both run this and exactly one matches a row.
--
-- Its real job is making the SIDE EFFECTS fire exactly once. The capacity
-- release, the booking_event and the outbox row all hang off a matched row, so
-- the caller that matches zero rows must skip all three — otherwise a retried
-- cancel would return a place twice and append a second event.
--
-- Zero rows is NOT automatically an error. It means "this call did not perform
-- the cancellation", and the caller then asks why. If the booking is already
-- CANCELLED the caller's intent is satisfied and the honest answer is the
-- cancelled booking; only a booking in some other terminal state is a conflict.
-- See booking.Cancel.
--
-- There is NO separate "release the slot" step, and that is the point of
-- non-negotiable #4. no_double_book's predicate covers only CONFIRMED, HELD and
-- BLOCKED rows, so moving to CANCELLED drops this row out of the constraint's
-- partial index and the slot is bookable again the instant the transaction
-- commits. Availability is derived, so there is no second field to update and
-- nothing that can disagree.
--
-- The prev CTE reads the pre-UPDATE snapshot, so from_status is the status the
-- booking actually had rather than one re-read afterwards.
WITH prev AS (
  SELECT id, status AS from_status
    FROM bookings
   WHERE id = $1
),
upd AS (
  UPDATE bookings
     SET status = 'CANCELLED'
   WHERE id = $1
     AND status IN ('CONFIRMED','HELD')
  RETURNING id, facility_id, user_id, is_exclusive,
            lower(during) AS starts, upper(during) AS ends, created_at
)
SELECT upd.id, upd.facility_id, upd.user_id, upd.is_exclusive,
       upd.starts, upd.ends, upd.created_at, prev.from_status::text
  FROM upd
  JOIN prev ON prev.id = upd.id;
