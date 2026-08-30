-- Read an existing attendance record.
--
-- Used only to disambiguate the zero-row case of checkin_redeem.sql: a row here
-- means the student has already checked in and their retry is satisfied. It is
-- never consulted BEFORE the insert — that would be the read-then-write gap, and
-- two simultaneous scans would both find nothing and both proceed. The primary
-- key decides; this only explains what it decided.
SELECT booking_id, at, method
  FROM check_ins
 WHERE booking_id = $1;
