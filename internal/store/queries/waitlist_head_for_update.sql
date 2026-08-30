-- SUPERSEDED by waitlist_claim_head.sql, which is what the promotion path
-- actually runs. This is the M0 sketch of the same statement, kept because it
-- is the shape §6.2 prints; if the two ever disagree, waitlist_claim_head.sql
-- is the one that decides who gets promoted.
--
-- The second concurrency proof. IMPLEMENTATION.md §6.2.
--
-- SKIP LOCKED so that concurrent cancellations promote DIFFERENT students
-- rather than contending for the same head-of-queue row.
SELECT id, user_id
  FROM waitlist
 WHERE facility_id = $1 AND during && $2::tstzrange AND status = 'WAITING'
 ORDER BY priority DESC, position ASC
   FOR UPDATE SKIP LOCKED
 LIMIT 1;
