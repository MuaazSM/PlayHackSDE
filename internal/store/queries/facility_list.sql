-- The catalogue, for the discovery screen. Small, static, and read constantly.
SELECT id, name, sport, is_exclusive, capacity,
       opens_at, closes_at, granularity, min_duration, max_duration, is_active
  FROM facilities
 WHERE is_active
 ORDER BY sport, name;
