-- A student's own bookings. IMPLEMENTATION.md §6.1.
--
-- Filtered to CONFIRMED and HELD so it matches idx_bookings_user_upcoming's
-- predicate exactly, and ordered by lower(during) so it matches the index's sort
-- order too — the index then serves both the filter and the ordering.
--
-- Cancelled bookings are deliberately absent: this backs the "my bookings"
-- screen, and the full history lives in booking_events.
SELECT b.id, b.facility_id, f.name, b.user_id,
       lower(b.during), upper(b.during), b.status::text, b.created_at
  FROM bookings b
  JOIN facilities f ON f.id = b.facility_id
 WHERE b.user_id = $1
   AND b.status IN ('CONFIRMED','HELD')
 ORDER BY lower(b.during);
