-- Migration 002 DOWN: drop games
-- moves references games via FK ON DELETE CASCADE, but the moves table is
-- dropped by migration 003 down before this runs. Dropping games here is safe.

DROP INDEX IF EXISTS idx_games_status;
DROP TABLE IF EXISTS games;