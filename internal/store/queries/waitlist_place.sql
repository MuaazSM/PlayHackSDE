-- How far up the queue an entry actually is — the number a student reads.
--
-- position (the bigserial) is a global ordering key, not a place: the first
-- person ever to queue for badminton might hold position 87. This counts the
-- WAITING entries that would be promoted before this one, plus itself, so the
-- head of the queue is 1.
--
-- "Ahead of me" is exactly waitlist_claim_head's ORDER BY read as a predicate:
-- higher priority first, then lower position. The row comparison spells that
-- once — (priority, -position) descending is the same order — so the two
-- statements cannot drift into disagreeing about who is next.
--
-- Returns 0 for an entry that is no longer WAITING (promoted, claimed, expired
-- or cancelled): it has no place in the queue any more. No row at all means no
-- such entry, which the caller reports as 404.
WITH me AS (
  SELECT id, facility_id, during, priority, position, status
    FROM waitlist
   WHERE id = $1
)
SELECT (
         SELECT count(*)
           FROM waitlist w
          WHERE w.facility_id = me.facility_id
            AND w.during && me.during
            AND w.status = 'WAITING'
            AND (w.priority, -w.position) >= (me.priority, -me.position)
       )::int AS place,
       me.status::text
  FROM me;
