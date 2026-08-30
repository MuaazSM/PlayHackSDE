-- An existing closure over EXACTLY this window, for retry convergence.
--
-- Non-negotiable #5 requires a retried write to return the original result, and
-- a closure cannot lean on uq_bookings_user_idem to do it: that index is keyed
-- on (user_id, idem_key) and a closure has no user, so two identical closures
-- would both be admitted (see closure_insert.sql).
--
-- This runs as the second statement of the closure transaction, immediately
-- after pg_advisory_xact_lock on the same facility — so no concurrent closure of
-- the same window can slip between this SELECT and the INSERT that follows it.
--
-- IT IS NOT THE READ-THEN-WRITE OF NON-NEGOTIABLE #2, on two counts. It is not
-- on the booking path — no student's request reaches it — and it does not decide
-- whether the closure is admitted. For an exclusive facility no_double_book
-- still decides: if this read misses, the INSERT raises 23P01 and the caller
-- gets a conflict, exactly as it would have without the read. All this can do is
-- turn a duplicate into a replay.
--
-- Equality, not overlap, on purpose. A closure that merely overlaps an existing
-- one is a different closure and must be answered on its own merits; only the
-- same window is the same request submitted twice.
SELECT id, created_at
  FROM bookings
 WHERE facility_id = $1
   AND status = 'BLOCKED'
   AND during = tstzrange($2::timestamptz, $3::timestamptz, '[)')
 LIMIT 1;
