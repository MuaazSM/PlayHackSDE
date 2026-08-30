-- Downgrade rows whose send failed after the claim committed. §8.
--
-- attempts was already incremented by outbox_drain, so nothing is added here —
-- this only records the verdict. The row is picked up again by
-- outbox_requeue_failed once its backoff has elapsed, and stays FAILED for good
-- once attempts reaches the ceiling.
UPDATE outbox
   SET status = 'FAILED'
 WHERE id = ANY($1)
RETURNING id;
