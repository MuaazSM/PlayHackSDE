-- Who holds the court, according to the database.
--
-- Read back rather than taken from the winning goroutine's return value. The
-- goroutine reports what it believes; this reports what committed. On stage they
-- are the same, and the point of asking twice is that nobody has to take the
-- first answer on trust.
SELECT b.id, b.user_id, u.roll_no
  FROM bookings b
  JOIN users u ON u.id = b.user_id
 WHERE b.facility_id = $1
   AND b.during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
   AND b.status = 'CONFIRMED'
 ORDER BY b.created_at
 LIMIT 1;
