-- Idempotent replay lookup. IMPLEMENTATION.md §4.5 and §4.8.
--
-- Runs ONLY after the write transaction has already rolled back. Once a
-- statement raises inside a transaction the transaction is aborted and no
-- further query runs on that connection, so this must execute on a fresh one.
-- Served by uq_bookings_user_idem, so it is a single index lookup.
SELECT id, facility_id, user_id, lower(during), upper(during),
       status::text, idem_key, created_at
  FROM bookings
 WHERE user_id = $1 AND idem_key = $2;
