-- Close out the queue entry whose offer was accepted. §6.3.
--
-- Runs in the same transaction as booking_claim_held.sql, so an entry cannot be
-- marked CLAIMED against a booking whose confirmation rolled back.
--
-- Zero rows is not an error: a booking may be claimed by an owner who reached
-- HELD some other way, and a repeated claim finds the entry already CLAIMED.
-- Both are states the caller's intent is already satisfied by.
UPDATE waitlist
   SET status = 'CLAIMED'
 WHERE booking_id = $1
   AND status = 'PROMOTED'
RETURNING id, user_id;
