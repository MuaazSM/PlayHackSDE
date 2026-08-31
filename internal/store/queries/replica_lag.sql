-- How far the availability replica trails the primary, in seconds.
-- IMPLEMENTATION.md §14, replica_lag_seconds.
--
-- pg_last_xact_replay_timestamp() is NULL on a primary and on a replica that has
-- not replayed anything yet. Both mean "nothing to be stale about", which is 0,
-- not unknown — a NULL here would show as a gap on the dashboard and read as a
-- broken exporter.
--
-- Clamped at zero: the primary's clock and the replica's are not the same clock,
-- and a few milliseconds of skew must not render as negative lag.
SELECT GREATEST(
         0,
         COALESCE(
           EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())),
           0
         )
       )::float8;
