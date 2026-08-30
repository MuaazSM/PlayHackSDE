-- 0003 — core tables. The exclusion constraint lands here.
--
-- ⚠ DELIBERATE DEVIATION from PRD §5.2 and CLAUDE.md, per IMPLEMENTATION.md §3.2.
--
-- The DDL as printed in the PRD scopes the exclusion predicate as
--     WHERE (status IN ('CONFIRMED','BLOCKED'))
-- which covers EVERY booking row, including rows for the gymnasium. With
-- capacity 30, the second person to book the 6 PM gym slot would be rejected by
-- the exclusion constraint before Mechanism B (atomic capacity decrement) ever
-- ran. The two mechanisms would fight and Mechanism B would be dead code.
--
-- Fix: carry is_exclusive on the booking row and put it in the predicate. A
-- partial-index predicate can only reference the row's own columns, so
-- denormalising the flag is not optional — it is the only way to scope the
-- constraint. The composite FK below pins the copy to its facility so it cannot
-- drift. This is the same mechanism, correctly scoped — not a new approach.

CREATE TABLE users (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  roll_no     text UNIQUE NOT NULL,
  name        text NOT NULL,
  email       citext UNIQUE NOT NULL,
  role        user_role NOT NULL DEFAULT 'STUDENT',
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE facilities (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,
  sport         text NOT NULL,
  is_exclusive  boolean NOT NULL,
  capacity      int NOT NULL DEFAULT 1 CHECK (capacity >= 1),
  opens_at      time NOT NULL,
  closes_at     time NOT NULL,
  granularity   interval NOT NULL DEFAULT '60 minutes',
  min_duration  interval NOT NULL DEFAULT '60 minutes',
  max_duration  interval NOT NULL DEFAULT '120 minutes',
  is_active     boolean NOT NULL DEFAULT true,

  CHECK (closes_at > opens_at),
  -- makes the two flags one fact; the facility row cannot be self-inconsistent
  CHECK (is_exclusive = (capacity = 1)),
  -- target for the composite FK from bookings; keeps the denormalised flag honest
  UNIQUE (id, is_exclusive)
);

CREATE TABLE bookings (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  facility_id   uuid NOT NULL REFERENCES facilities(id),
  user_id       uuid REFERENCES users(id),          -- NULL for closures
  is_exclusive  boolean NOT NULL,                   -- denormalised; see FK below
  during        tstzrange NOT NULL,
  status        booking_status NOT NULL DEFAULT 'CONFIRMED',
  idem_key      text,
  held_until    timestamptz,                        -- set only while status = 'HELD'
  created_at    timestamptz NOT NULL DEFAULT now(),

  FOREIGN KEY (facility_id, is_exclusive)
    REFERENCES facilities(id, is_exclusive),

  CHECK (NOT isempty(during)),
  CHECK ((status = 'BLOCKED') = (user_id IS NULL)),
  CHECK ((status = 'HELD') = (held_until IS NOT NULL)),

  -- Mechanism A. Always build `during` as tstzrange(start, end, '[)') so that
  -- 18:00-19:00 and 19:00-20:00 do NOT overlap.
  CONSTRAINT no_double_book
    EXCLUDE USING gist (facility_id WITH =, during WITH &&)
    WHERE (is_exclusive AND status IN ('CONFIRMED','HELD','BLOCKED'))
);

-- Idempotency. A replayed Idempotency-Key returns the original booking (23505).
CREATE UNIQUE INDEX uq_bookings_user_idem
  ON bookings (user_id, idem_key) WHERE idem_key IS NOT NULL;
