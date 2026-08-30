-- Record attendance, guarded by the check-in window. IMPLEMENTATION.md §7.
--
-- The window is checked HERE, in Postgres' now(), not in Go. A student arriving
-- one second either side of the grace deadline must not have that decided by
-- whichever API replica's clock happened to serve them — the same reasoning that
-- puts held_until > now() inside booking_claim_held.sql's guard.
--
-- ON CONFLICT (booking_id) DO NOTHING is the idempotency. The primary key on
-- check_ins means a second scan of the same QR — a student tapping twice, or a
-- retried request whose first response was lost — writes nothing and reports
-- zero rows. The caller then re-reads the existing row and returns it, so both
-- calls answer 200 and exactly one row exists (non-negotiable #5).
--
-- Zero rows is therefore NOT automatically a failure. It means "this call did
-- not record the attendance", and the caller asks why: an existing check_ins row
-- is a satisfied retry, no row is a booking outside its window or in a status
-- that cannot be attended.
--
-- Only CONFIRMED bookings can be attended. A HELD row is an unclaimed promotion
-- offer, and a CANCELLED or NO_SHOW row has already released its court — letting
-- either be checked into would put somebody on a court the system has told
-- everybody else is free.
--
-- $1 booking, $2 method, $3 token id (for the audit trail; the token itself is
-- NEVER stored — it is a keyed hash of the minute and there is nothing to keep),
-- $4 how many seconds before the start a student may check in,
-- $5 how many seconds after the start they still may (GRACE_PERIOD_MIN).
INSERT INTO check_ins (booking_id, method, token_id)
SELECT b.id, $2, $3
  FROM bookings b
 WHERE b.id = $1
   AND b.status = 'CONFIRMED'
   AND now() >= lower(b.during) - make_interval(secs => $4::double precision)
   AND now() <= lower(b.during) + make_interval(secs => $5::double precision)
    ON CONFLICT (booking_id) DO NOTHING
RETURNING booking_id, at, method;
