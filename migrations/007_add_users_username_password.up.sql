-- Migration 007 UP: username/password login (PHASE_3.md addendum — fixes
-- TD-P3-007: POST /matchmaking/queue can never create a users row, since
-- matchmaking-service never touches Postgres by design, so a userID that
-- has never gone through POST /games at least once has no row at all and
-- CreateMatchedGame's FK constraint on users fails deterministically for
-- it. This lets a client obtain/recover a stable userID without going
-- through game creation first.
--
-- Nullable: existing and future anonymous users (created via POST /games'
-- CreateOrGetUser upsert path, migration 001's original design) have
-- neither column set. A user only gets both by going through POST /users
-- (AuthHandler.Register) instead — the two identity paths coexist, this
-- migration does not retire the anonymous one.
--
-- CHECK constraint enforces both-or-neither at the DB level rather than
-- relying on every current and future INSERT path to keep the two columns
-- consistent by convention alone.
--
-- password_hash stores a bcrypt hash (internal/auth.HashPassword) —
-- plaintext is never written here under any code path.

ALTER TABLE users
    ADD COLUMN username TEXT UNIQUE,
    ADD COLUMN password_hash TEXT,
    ADD CONSTRAINT users_username_password_hash_together
        CHECK ((username IS NULL) = (password_hash IS NULL));
