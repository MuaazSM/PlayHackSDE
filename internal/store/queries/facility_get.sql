-- Catalogue lookup for the booking write path.
--
-- Read through a 60s in-process cache (internal/facility). That is a cache of
-- CONFIGURATION, not of occupancy — availability is always derived from the
-- bookings table at read time, so non-negotiable #3 is untouched.
SELECT id, name, sport, is_exclusive, capacity,
       opens_at, closes_at, granularity, min_duration, max_duration, is_active
  FROM facilities
 WHERE id = $1;
