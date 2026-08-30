-- Return failed side effects to the queue once their backoff has elapsed. §8.
--
-- Exponential on attempts: base * 2^(attempts-1), capped. sent_at is the last
-- attempt's timestamp (outbox_drain stamps it on every claim), so the delay is
-- measured from the failure rather than from when the row was written — a row
-- that failed at attempt 4 does not get four retries the instant it is enqueued.
--
-- attempts < $1 is the dead-letter boundary. A row that has exhausted its
-- attempts stays FAILED and is left for a human: at-least-once delivery is a
-- promise about retries, not about infinite ones.
--
-- FOR UPDATE SKIP LOCKED for the same reason as the drain — two dispatchers
-- requeue different rows rather than blocking on each other.
UPDATE outbox
   SET status = 'PENDING'
 WHERE id IN (
   SELECT id
     FROM outbox
    WHERE status = 'FAILED'
      AND attempts < $1
      AND coalesce(sent_at, created_at)
            + make_interval(secs => least($2::float8 * power(2, attempts - 1), $3::float8))
          <= now()
    ORDER BY created_at, id
      FOR UPDATE SKIP LOCKED
    LIMIT $4)
RETURNING id;
