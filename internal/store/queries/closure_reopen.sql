-- Withdraw a closure, guarded by status. The mirror of booking_cancel.sql, and
-- for the same reason: two simultaneous reopens both run this and exactly one
-- matches a row, so the capacity restoration, the audit event and the outbox row
-- all fire exactly once.
--
-- THERE IS NO "UNBLOCK THE SLOT" STEP. no_double_book's predicate covers only
-- CONFIRMED, HELD and BLOCKED, so moving to CANCELLED drops this row out of the
-- constraint's partial index and the window is bookable again the instant this
-- transaction commits. For an exclusive facility that is the whole reopen;
-- shared facilities additionally need their counters restored, because Mechanism
-- B keeps a number rather than deriving from the rows.
--
-- status = 'BLOCKED' in the guard is also what stops this endpoint being a way to
-- cancel somebody's booking without going through the cancel path, its
-- authorisation and its notifications.
--
-- The CANCELLED row keeps user_id NULL, which migration 0009 admits explicitly.
UPDATE bookings
   SET status = 'CANCELLED'
 WHERE id = $1
   AND status = 'BLOCKED'
RETURNING id, facility_id, is_exclusive,
          lower(during), upper(during), created_at;
