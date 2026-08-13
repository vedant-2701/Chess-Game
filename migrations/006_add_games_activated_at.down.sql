-- Migration 006 DOWN

ALTER TABLE games
    DROP COLUMN IF EXISTS activated_at;
