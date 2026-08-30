-- Retire the queue entry behind an offer nobody claimed. §6.3.
--
-- Runs in the sweeper's transaction, alongside the HELD → CANCELLED transition
-- of the booking it points at, so the entry and the court are released
-- together. An entry marked EXPIRED while its hold survived would be a court
-- reserved for a student who is no longer being offered it.
UPDATE waitlist
   SET status = 'EXPIRED'
 WHERE booking_id = $1
   AND status = 'PROMOTED'
RETURNING id, user_id;
