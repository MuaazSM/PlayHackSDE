-- Claim the head of the queue for a freed window. IMPLEMENTATION.md §6.2.
--
-- SKIP LOCKED is what lets three simultaneous cancellations promote three
-- students AT THE SAME TIME rather than one behind another. It is worth being
-- precise about what it does and does not buy, because the two are easy to
-- conflate and the difference was measured rather than assumed:
--
--   WHAT KEEPS THE PROMOTIONS DISTINCT is the row lock plus the
--   `status = 'WAITING'` predicate. A second claimer cannot be handed a row
--   somebody else is already promoting: it either steps over it (with SKIP
--   LOCKED) or waits for it, re-checks the predicate under READ COMMITTED,
--   finds it PROMOTED and moves to the next one (without). Tried both ways on
--   this schema — plain FOR UPDATE does not hand one student two courts.
--
--   WHAT SKIP LOCKED BUYS is that claimers never queue behind each other. With
--   plain FOR UPDATE the second and third cancels BLOCK on the head row for as
--   long as the first cancel's transaction runs, holding their own
--   transactions — and their own student's cancellation — open while they
--   wait. Three independent cancellations become a serial chain, one slow
--   cancel stalls every other cancel on that facility, and the sweeper, which
--   holds a transaction across a whole batch, would stall live cancels
--   outright.
--
-- So correctness survives without it and responsiveness does not, in a system
-- whose whole premise is that losing stays fast. It is the same reasoning as
-- the advisory lock on the write path, pointed the other way: there contention
-- is shaped by serialising deliberately, here by refusing to.
--
-- TestPromotion_SkipsLockedEntries is the mutation test — hold the head row in
-- another transaction and a cancel must still promote somebody, promptly.
--
-- ORDERING. priority DESC then position ASC: priority tiers first, FIFO within
-- a tier. position is the bigserial from migration 0005, so the queue orders
-- itself — there is no counter to increment and therefore no race to order it.
-- idx_waitlist_head is built on exactly this order.
--
-- LIMIT 1 because one freed window promotes one student. The row lock is held
-- for the remainder of the caller's transaction — which is the cancelling
-- transaction, so a promotion cannot outlive a cancel that rolled back.
--
-- $1 facility, $2/$3 the freed window as [start, end).
SELECT id, user_id, priority, position
  FROM waitlist
 WHERE facility_id = $1
   AND during && tstzrange($2::timestamptz, $3::timestamptz, '[)')
   AND status = 'WAITING'
 ORDER BY priority DESC, position ASC
   FOR UPDATE SKIP LOCKED
 LIMIT 1;
