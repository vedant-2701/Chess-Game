-- Migration 001 UP: users
-- Minimal anonymous user identity. No credentials, no email.
-- userID is generated client-side (UUID v4) and submitted on first game creation.
-- This table exists so games.player_white_id and games.player_black_id have a valid FK target.

CREATE TABLE users (
    id         UUID PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);