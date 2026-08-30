-- Claim a batch of pending side effects. IMPLEMENTATION.md §8.
--
-- One statement, and the claim IS the update: the rows leave PENDING in the same
-- breath they are read, so a second dispatcher cannot pick them up. SKIP LOCKED
-- is what lets two dispatchers work the same queue concurrently instead of one
-- queueing behind the other's batch.
--
-- attempts is incremented here, before the send, not after it. A send that never
-- returns must still count as an attempt, or a permanently failing notification
-- would be retried forever at full speed.
--
-- The send happens AFTER this transaction commits. Nothing in a transaction may
-- make a network call (CLAUDE.md, Conventions/Transactions).
UPDATE outbox
   SET status = 'SENT',
       attempts = attempts + 1,
       sent_at = now()
 WHERE id IN (
   SELECT id
     FROM outbox
    WHERE status = 'PENDING'
    ORDER BY created_at, id
      FOR UPDATE SKIP LOCKED
    LIMIT $1)
RETURNING id, topic, payload, attempts;
