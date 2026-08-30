-- Accept a promotion offer: HELD → CONFIRMED. IMPLEMENTATION.md §6.3.
--
-- The guard is the concurrency control, exactly as in booking_cancel.sql. A
-- claim racing the sweeper both run this and the guard decides: either the
-- sweeper's UPDATE to CANCELLED lands first and this matches zero rows, or this
-- lands first and the sweeper's `status = 'HELD'` filter no longer matches.
-- Neither can half-happen, and neither needs to read the other's state first.
--
-- held_until > now() is inside the guard, not checked in Go, for the same
-- reason: a claim that arrives one millisecond before expiry must not be
-- decided by whichever clock the API replica happens to have. Postgres owns
-- now().
--
-- held_until is cleared because the row is no longer a hold, and because the
-- table's CHECK ((status = 'HELD') = (held_until IS NOT NULL)) makes leaving it
-- set impossible rather than merely untidy.
--
-- The slot stays reserved throughout: HELD and CONFIRMED are both inside
-- no_double_book's predicate, so this transition never opens a window in which
-- somebody else could take the court.
WITH prev AS (
  SELECT id, status AS from_status
    FROM bookings
   WHERE id = $1
),
upd AS (
  UPDATE bookings
     SET status = 'CONFIRMED',
         held_until = NULL
   WHERE id = $1
     AND status = 'HELD'
     AND held_until > now()
  RETURNING id, facility_id, user_id, is_exclusive,
            lower(during) AS starts, upper(during) AS ends, created_at
)
SELECT upd.id, upd.facility_id, upd.user_id, upd.is_exclusive,
       upd.starts, upd.ends, upd.created_at, prev.from_status::text
  FROM upd
  JOIN prev ON prev.id = upd.id;
