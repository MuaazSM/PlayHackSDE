-- 0008 — a facility's exclusivity is immutable.
--
-- bookings.is_exclusive is a denormalised copy of facilities.is_exclusive. The
-- copy has to exist: a partial-index predicate can only reference the row's own
-- columns, so scoping no_double_book required carrying the flag (migration
-- 0003), and the composite FK keeps a NEW row honest.
--
-- What the FK cannot do is keep an OLD row honest. FOREIGN KEY (facility_id,
-- is_exclusive) REFERENCES facilities(id, is_exclusive) has no ON UPDATE CASCADE
-- and would reject the parent change — but the deeper problem is that cascading
-- would be WRONG. Flipping a live facility does not merely rewrite a flag; it
-- reinterprets every existing booking's release path, silently sending
-- exclusive bookings down the capacity-counter branch on cancel, and shared ones
-- into an exclusion constraint they were never in.
--
-- So exclusivity is fixed at creation. Converting a court into a shared hall
-- means creating a new facility, which is the honest operation anyway: the
-- capacity model differs and old bookings should not be reinterpreted under it.
--
-- capacity itself stays mutable — a gym may grow from 30 to 40 — as long as the
-- CHECK (is_exclusive = (capacity = 1)) still holds.
CREATE FUNCTION facilities_exclusivity_is_immutable() RETURNS trigger AS $$
BEGIN
  IF NEW.is_exclusive IS DISTINCT FROM OLD.is_exclusive THEN
    RAISE EXCEPTION
      'facilities.is_exclusive is immutable (facility %: % -> %)',
      OLD.id, OLD.is_exclusive, NEW.is_exclusive
      USING ERRCODE = 'restrict_violation',
            HINT = 'Create a new facility instead; existing bookings carry a denormalised copy of this flag.';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER facilities_exclusivity_immutable
  BEFORE UPDATE ON facilities
  FOR EACH ROW EXECUTE FUNCTION facilities_exclusivity_is_immutable();
