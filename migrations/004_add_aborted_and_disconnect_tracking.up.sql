-- Migration 004 UP: ABORTED status + disconnect timestamp tracking
--
-- ABORTED (DECISIONS_LOG_PHASE_2.md ADR-029): a game whose creator connected
-- but whose opponent never joined at all before the abandonment timer fired.
-- Distinct from ABANDONED, which requires the game to have actually reached
-- ACTIVE (both players joined) before ending in a mutual-disconnect draw. An
-- ABORTED game never started: no outcome, no outcome_reason — it was void,
-- not a scored draw.
--
-- white_disconnected_at / black_disconnected_at (ADR-030): persisted
-- alongside the in-memory abandonment timer (Manager.abandonTimers), which
-- is pure per-process state and does not survive an instance dying mid-grace
-- period. A surviving instance that later hydrates this game reads these
-- columns to correctly resume (or immediately resolve) the grace period
-- instead of silently losing track of it entirely. NULL means "not
-- currently in a disconnect grace period." Set on disconnect, cleared on
-- reconnect.

ALTER TABLE games
    DROP CONSTRAINT games_status_check;

ALTER TABLE games
    ADD CONSTRAINT games_status_check
    CHECK (status IN ('WAITING_FOR_PLAYER', 'ACTIVE', 'COMPLETED', 'ABANDONED', 'ABORTED'));

ALTER TABLE games
    ADD COLUMN white_disconnected_at TIMESTAMPTZ,
    ADD COLUMN black_disconnected_at TIMESTAMPTZ;
