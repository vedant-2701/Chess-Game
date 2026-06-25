-- Migration 002 UP: games

CREATE TABLE games (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    status          TEXT        NOT NULL DEFAULT 'WAITING_FOR_PLAYER'
                                CHECK (status IN ('WAITING_FOR_PLAYER', 'ACTIVE', 'COMPLETED', 'ABANDONED')),
    player_white_id UUID        NOT NULL REFERENCES users(id),
    player_black_id UUID        REFERENCES users(id),       -- NULL until second player joins
    current_fen     TEXT        NOT NULL DEFAULT 'rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1',
    white_time_ms   INTEGER     NOT NULL DEFAULT 600000,    -- 10 minutes in milliseconds
    black_time_ms   INTEGER     NOT NULL DEFAULT 600000,
    outcome         TEXT        CHECK (outcome IN ('WHITE', 'BLACK', 'DRAW')),
    outcome_reason  TEXT        CHECK (outcome_reason IN (
                                    'CHECKMATE', 'STALEMATE', 'RESIGNATION',
                                    'TIMEOUT', 'DRAW_AGREEMENT', 'ABANDONED'
                                )),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Partial index: only index games that are still in-flight.
-- COMPLETED and ABANDONED games are never queried by status at runtime.
-- This index is used by GetActiveGames on server restart and by the matchmaking
-- sanity checks added in later phases.
CREATE INDEX idx_games_status
    ON games (status)
    WHERE status IN ('WAITING_FOR_PLAYER', 'ACTIVE');