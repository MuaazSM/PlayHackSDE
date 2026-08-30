-- 0001 — extensions.
--
-- btree_gist is the hour-one gate (IMPLEMENTATION.md §3.1). Without it the
-- exclusion constraint in 0003 cannot mix an equality operator (facility_id
-- WITH =) with a range operator (during WITH &&) in one GiST index, and the
-- whole concurrency design falls back to something materially weaker.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- gen_random_uuid() for primary keys.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- case-insensitive email on users.
CREATE EXTENSION IF NOT EXISTS citext;
