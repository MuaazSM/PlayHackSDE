-- 0004 — slot_capacity. Mechanism B, shared facilities only.
--
-- Shared facilities are grid-aligned BY CONSTRUCTION: one counter row exists per
-- (facility, slot_start). A shared booking's `during` must therefore start on a
-- granularity boundary and span a whole number of slots; the server rejects
-- anything else with 422 SLOT_NOT_ALIGNED. Exclusive facilities keep full
-- variable-duration freedom. This asymmetry is a deliberate trade, not an
-- oversight — see IMPLEMENTATION.md §3.3.
--
-- Rows are created lazily by the upsert in §4.4. No nightly job pre-materialises
-- a grid.
CREATE TABLE slot_capacity (
  facility_id  uuid NOT NULL REFERENCES facilities(id),
  slot_start   timestamptz NOT NULL,
  slot_end     timestamptz NOT NULL,
  capacity     int NOT NULL CHECK (capacity >= 0),
  booked       int NOT NULL DEFAULT 0 CHECK (booked >= 0),
  PRIMARY KEY (facility_id, slot_start),
  CHECK (booked <= capacity),
  CHECK (slot_end > slot_start)
);
