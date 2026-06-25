-- Migration 003 UP: moves

CREATE TABLE moves (
    id          BIGSERIAL   PRIMARY KEY,
    game_id     UUID        NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    move_number INT         NOT NULL,
    color       TEXT        NOT NULL CHECK (color IN ('WHITE', 'BLACK')),
    san         TEXT        NOT NULL,    -- Standard Algebraic Notation: "e4", "Nf3", "O-O"
    fen_after   TEXT        NOT NULL,    -- Board state after this move (used for replay and Phase 4 analysis)
    played_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enforces that move numbers within a game are unique and correctly sequenced.
-- Also serves as the primary lookup path: all move queries filter by game_id first.
CREATE UNIQUE INDEX idx_moves_game_move
    ON moves (game_id, move_number);

-- Separate non-unique index on game_id alone for GetMovesForGame queries.
-- The UNIQUE index above could serve game_id lookups via index scan on the leading
-- column, but an explicit index makes the intent clear and avoids relying on
-- that optimisation detail. Revisit in Phase 4 with EXPLAIN ANALYZE.
CREATE INDEX idx_moves_game_id
    ON moves (game_id);