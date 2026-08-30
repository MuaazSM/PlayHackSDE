-- Load one booking, for authorisation and for the window a cancel must release.
--
-- This is a read of the row about to be mutated, not a read of availability.
-- The concurrency control for cancel is the status-guarded UPDATE in
-- booking_cancel.sql; nothing here decides whether the cancel succeeds.
SELECT b.id, b.facility_id, b.user_id, b.is_exclusive,
       lower(b.during), upper(b.during), b.status::text, b.idem_key, b.created_at
  FROM bookings b
 WHERE b.id = $1;
