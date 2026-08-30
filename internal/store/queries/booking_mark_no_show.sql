-- Release the courts nobody turned up for. IMPLEMENTATION.md §7.
--
-- Every minute: any CONFIRMED booking whose start is further in the past than
-- the grace period, and which has no attendance record, becomes NO_SHOW.
--
-- THERE IS NO "RELEASE THE SLOT" STEP for an exclusive facility, and its absence
-- is the same point non-negotiable #4 makes on the cancel path. NO_SHOW is
-- outside no_double_book's predicate (CONFIRMED, HELD, BLOCKED), so the status
-- change alone makes the court bookable again the instant this commits. Shared
-- facilities are the exception only because Mechanism B keeps a counter rather
-- than deriving from the rows; the caller decrements it in this transaction.
--
-- NOT EXISTS against check_ins rather than a flag on the booking. Attendance is
-- a fact recorded in one table, so there is no second field that could disagree
-- with it — the same reason availability has no is_available column.
--
-- FOR UPDATE SKIP LOCKED on the inner select is what makes two sweepers safe to
-- run at once: they take disjoint sets of rows instead of queueing behind each
-- other, and each booking is handled exactly once. Without SKIP LOCKED the
-- second sweeper would still be CORRECT — it would block, then re-check
-- `status = 'CONFIRMED'` under READ COMMITTED and find the row already swept —
-- but it would hold its whole batch open waiting for the first, which for a
-- worker that also promotes off the waitlist means stalling live cancellations.
--
-- ORDER BY lower(during) so the longest-overdue courts are freed first, and so
-- two sweepers walk the rows in the same order.
--
-- $1 grace period in seconds (GRACE_PERIOD_MIN), $2 bounds the batch.
UPDATE bookings b
   SET status = 'NO_SHOW'
 WHERE b.id IN (
         SELECT x.id
           FROM bookings x
          WHERE x.status = 'CONFIRMED'
            AND lower(x.during) < now() - make_interval(secs => $1::double precision)
            AND NOT EXISTS (SELECT 1 FROM check_ins c WHERE c.booking_id = x.id)
          ORDER BY lower(x.during)
            FOR UPDATE SKIP LOCKED
          LIMIT $2
       )
RETURNING b.id, b.facility_id, b.user_id, b.is_exclusive,
          lower(b.during) AS starts, upper(b.during) AS ends;
