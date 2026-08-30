-- Reverse 0010. Dropping the column first, then the type it depends on.
ALTER TABLE users DROP COLUMN tier;

DROP TYPE priority_tier;
