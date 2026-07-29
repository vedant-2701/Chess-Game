-- Migration 004 DOWN
--
-- NOTE: this will fail if any row currently has status = 'ABORTED' (the
-- narrower CHECK constraint being restored would reject it) — standard
-- down-migration caveat, not a bug. Resolve or delete such rows first if you
-- need to roll back past this migration.

ALTER TABLE games
    DROP COLUMN IF EXISTS white_disconnected_at,
    DROP COLUMN IF EXISTS black_disconnected_at;

ALTER TABLE games
    DROP CONSTRAINT games_status_check;

ALTER TABLE games
    ADD CONSTRAINT games_status_check
    CHECK (status IN ('WAITING_FOR_PLAYER', 'ACTIVE', 'COMPLETED', 'ABANDONED'));
