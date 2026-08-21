-- Migration 007 DOWN

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_username_password_hash_together,
    DROP COLUMN IF EXISTS username,
    DROP COLUMN IF EXISTS password_hash;
