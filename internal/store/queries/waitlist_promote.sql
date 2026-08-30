-- Record the offer against the waitlist entry. IMPLEMENTATION.md §6.2 step 3.
--
-- Runs in the same transaction as the HELD insert it points at, so the entry
-- can never claim an offer whose booking rolled back.
--
-- offer_expires mirrors the booking's held_until deliberately: they are one
-- fact seen from two tables, and the claim path checks held_until — the column
-- the exclusion constraint actually cares about — so the copy here is for
-- display and for the sweeper's join, never for a decision.
--
-- The status guard is belt and braces: the caller already holds this row's lock
-- from waitlist_claim_head, which selected it WHERE status = 'WAITING'.
UPDATE waitlist
   SET status = 'PROMOTED',
       booking_id = $2,
       offer_expires = $3
 WHERE id = $1
   AND status = 'WAITING'
RETURNING id, position;
