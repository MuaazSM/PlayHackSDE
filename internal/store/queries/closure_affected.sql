-- Students whose bookings sit inside a closed window. §10.4 step 3.
--
-- THIS QUERY DOES NOT CANCEL ANYTHING, and that is deliberate. Revoking somebody's
-- court is a decision with a person on the other end of it — a rescheduled match,
-- a message to send — so the system surfaces the list and a human acts on it
-- through the ordinary cancel path, which already notifies, releases capacity and
-- promotes the queue. Auto-cancelling here would do all of that silently, from a
-- transaction the manager cannot review.
--
-- Two callers, both after the fact:
--
--   * the closure succeeded (SHARED facility, where a BLOCKED row and existing
--     bookings coexist by design) — these are the bookings staff must resolve;
--   * the closure was REJECTED by no_double_book (exclusive facility) — these are
--     the conflicting bookings the 409 lists. That call runs after the
--     transaction has rolled back, on a fresh connection, per §4.5.
--
-- $1 facility id, $2 window start, $3 window end.
SELECT b.id,
       b.user_id,
       u.roll_no,
       u.name,
       lower(b.during),
       upper(b.during),
       b.status::text
  FROM bookings b
  JOIN users u ON u.id = b.user_id
 WHERE b.facility_id = $1
   AND b.status IN ('CONFIRMED', 'HELD')
   AND b.during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
 ORDER BY lower(b.during), b.id;
