-- 0007 — read-path indexes.
--
-- The exclusion constraint's own GiST index already serves availability lookups
-- for exclusive facilities, so nothing here duplicates it.

CREATE INDEX idx_bookings_user_upcoming
  ON bookings (user_id, lower(during))
  WHERE status IN ('CONFIRMED','HELD');

-- NOT redundant with the constraint index: that one is partial on is_exclusive,
-- so shared-facility rows are not in it.
CREATE INDEX idx_bookings_day
  ON bookings USING gist (facility_id, during)
  WHERE status IN ('CONFIRMED','HELD','BLOCKED');

CREATE INDEX idx_waitlist_head
  ON waitlist (facility_id, priority DESC, position)
  WHERE status = 'WAITING';

CREATE INDEX idx_outbox_pending
  ON outbox (created_at) WHERE status = 'PENDING';

CREATE INDEX idx_bookings_noshow_sweep
  ON bookings (lower(during)) WHERE status = 'CONFIRMED';
