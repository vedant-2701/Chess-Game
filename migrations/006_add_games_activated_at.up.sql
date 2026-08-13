-- Migration 006 UP: games.activated_at (DECISIONS_LOG_PHASE_3.md ADR-041)
--
-- Anchors the first-move grace period's White-window remaining-duration
-- computation at hydration — the same role white_disconnected_at/
-- black_disconnected_at play for the ordinary per-color abandon timer
-- (ADR-030). Set once, atomically with the same statement that persists the
-- WAITING→ACTIVE status transition (GameStore.ActivateGame) — never touched
-- again after that.
--
-- Nullable, and deliberately not backfilled for existing rows: a NULL
-- activated_at on a genuinely ACTIVE game is a real, if narrow, possible
-- outcome of that same statement's non-fatal write failing for that
-- specific game, not only a hypothetical pre-migration row. The hydration
-- code path (Manager.armTimersForGameStatus) has an explicit fallback for
-- this — see effectiveWindowStartedAt.

ALTER TABLE games
    ADD COLUMN activated_at TIMESTAMPTZ;
