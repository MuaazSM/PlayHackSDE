-- The pool of students the race fires as. IMPLEMENTATION.md §13.
--
-- Read-only: the race console never creates accounts. It races as whoever the
-- seed put in the database, so a demo run leaves the user table exactly as it
-- found it and `make seed` is the only thing that decides who exists.
--
-- Ordered by roll number so the same n produces the same bookers every run —
-- a demo that names a different winner for reasons unrelated to the race is
-- harder to trust, not easier.
SELECT id, roll_no
  FROM users
 WHERE role = 'STUDENT'
 ORDER BY roll_no
 LIMIT $1;
