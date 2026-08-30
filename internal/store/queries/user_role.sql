-- The actor's role, for authorising an action on someone else's booking.
SELECT role::text FROM users WHERE id = $1;
