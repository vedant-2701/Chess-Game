-- Migration 005 DOWN

ALTER TABLE games
    DROP COLUMN IF EXISTS matchmaking_request_id;
