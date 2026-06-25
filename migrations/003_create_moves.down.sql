-- Migration 003 DOWN: drop moves
-- Drop indexes explicitly before the table, though DROP TABLE also drops
-- associated indexes. Being explicit avoids ambiguity and matches the up migration.

DROP INDEX IF EXISTS idx_moves_game_id;
DROP INDEX IF EXISTS idx_moves_game_move;
DROP TABLE IF EXISTS moves;