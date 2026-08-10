-- Migration 005 UP: matchmaking_request_id idempotency column
--
-- DECISIONS_LOG_PHASE_3.md ADR-034 (rationale corrected by ADR-039):
-- correlation/idempotency token for CreateMatchedGame's atomic two-player
-- INSERT. Generated once by chess-server per ZPOPMIN-won pairing attempt,
-- reused across every retry of that same attempt, and checked via
-- ON CONFLICT (matchmaking_request_id) DO NOTHING — this is what makes a
-- retry after an ambiguous failure (timeout / lost ACK) safe rather than a
-- double-booking risk (two players matched into a live game AND
-- re-enqueued by a caller who wrongly believes the first attempt failed).
--
-- Nullable: only matchmaking-originated games populate it. Shared-link
-- games (CreateGame/UpdatePlayerBlack) never set it and remain NULL for
-- the life of the row.
--
-- UUID, but deliberately NOT the same generation convention as games.id
-- itself (UUID v7 — see internal/game/manager.go's CreateGame,
-- uuid.NewV7()). Per ADR-039: this column is only ever checked by exact
-- match (ON CONFLICT / a future point lookup), never range-scanned or
-- ordered, so v7's B-tree-locality benefit does not apply here — use v4.
--
-- UNIQUE (not just an index) is required: Postgres needs a unique
-- constraint or unique index backing a column before it can be named as
-- an ON CONFLICT target. UNIQUE gives both the constraint and its backing
-- index in one declaration.

ALTER TABLE games
    ADD COLUMN matchmaking_request_id UUID UNIQUE;
