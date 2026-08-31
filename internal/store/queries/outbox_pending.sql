-- Depth of the side-effect queue. IMPLEMENTATION.md §14, outbox_pending.
--
-- A gauge input, sampled once per dispatcher pass, not a hot query. PENDING only:
-- FAILED rows are requeued to PENDING before the next drain, so counting them
-- here would double-count the same backlog.
SELECT count(*) FROM outbox WHERE status = 'PENDING';
