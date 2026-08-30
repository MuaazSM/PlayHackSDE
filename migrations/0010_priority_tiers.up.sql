-- 0010 — priority tiers (IMPLEMENTATION.md §11).
--
-- §11 requires waitlist.priority to be fed from a tier: institute team practice
-- (2) > hostel team (1) > individual (0). Nothing in migrations 0001-0009
-- records which of those a student is. `users.role` is about AUTHORITY (who may
-- close a court, who may mint a check-in token) and cannot be borrowed for this
-- without making one column mean two unrelated things — a STUDENT can be on the
-- institute team, and a MANAGER is not a higher-priority queuer.
--
-- So: one additive column, defaulted, on a table with a handful of rows. Every
-- existing user becomes INDIVIDUAL, which is the tier they already had
-- implicitly (waitlist.Join hardcoded priority 0 before this migration), so the
-- change is behaviour-preserving on existing data.
--
-- An enum rather than a bare int: the ORDER of the tiers is policy, expressed
-- once in Go (policy.Tier.Base), and a stored integer would invite a second,
-- silently different ranking to appear in SQL later. The database records WHICH
-- TIER a student is; the ranking is derived, in one place, the same way
-- availability is derived rather than stored.
CREATE TYPE priority_tier AS ENUM ('INDIVIDUAL', 'HOSTEL_TEAM', 'INSTITUTE_TEAM');

ALTER TABLE users
  ADD COLUMN tier priority_tier NOT NULL DEFAULT 'INDIVIDUAL';
