-- Leave the queue. The student's half of §6.2.
--
-- One statement, so there is no window in which the entry is read as WAITING
-- and then updated after a concurrent promotion has already taken it. The
-- guarded UPDATE is the concurrency control; the me CTE reads the PRE-update
-- snapshot and exists only to shape the error — 404 for no such entry, 403 for
-- somebody else's, 409 for an entry that is no longer waiting.
--
-- Only a WAITING entry may be abandoned this way. A PROMOTED entry owns a HELD
-- booking that reserves a court, and releasing that court is a cancellation of
-- the booking, not a queue edit: DELETE /bookings/:id is the honest path, and
-- it runs the promotion machinery so the next student gets the slot.
WITH me AS (
  SELECT id, user_id, status
    FROM waitlist
   WHERE id = $1
),
upd AS (
  UPDATE waitlist
     SET status = 'CANCELLED'
   WHERE id = $1
     AND user_id = $2
     AND status = 'WAITING'
  RETURNING id
)
SELECT me.user_id,
       me.status::text,
       (SELECT count(*) FROM upd)::int AS left_queue
  FROM me;
