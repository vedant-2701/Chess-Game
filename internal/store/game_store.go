package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GameStore handles persistence for the games table.
type GameStore struct {
	pool *pgxpool.Pool
}

// NewGameStore constructs a GameStore backed by the given pool.
func NewGameStore(pool *pgxpool.Pool) *GameStore {
	return &GameStore{pool: pool}
}

// scanGame reads a game row from the provided scan function.
//
// Both pgx.Row.Scan and pgx.Rows.Scan satisfy func(...any) error, so this
// helper works for single-row queries (QueryRow) and multi-row queries (Query):
//
//	scanGame(pool.QueryRow(ctx, q, id).Scan)   // single row
//	scanGame(rows.Scan)                         // inside rows iteration loop
//
// Nullable columns (player_black_id, outcome, outcome_reason) are scanned into
// *string intermediates and then converted to the typed pointer fields on Game.
// This avoids relying on pgx/v5's reflection-based custom type conversion, which
// is not guaranteed for user-defined string types.
func scanGame(scanFn func(dest ...any) error) (*Game, error) {
	var (
		g                    Game
		statusStr            string
		playerBlackID        *string
		outcome              *string
		outcomeReason        *string
		matchmakingRequestID *string
	)

	err := scanFn(
		&g.ID,
		&statusStr,
		&g.PlayerWhiteID,
		&playerBlackID,
		&g.CurrentFEN,
		&g.WhiteTimeMs,
		&g.BlackTimeMs,
		&outcome,
		&outcomeReason,
		&g.WhiteDisconnectedAt,
		&g.BlackDisconnectedAt,
		&matchmakingRequestID,
		&g.CreatedAt,
		&g.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	g.Status = GameStatus(statusStr)
	g.PlayerBlackID = playerBlackID
	g.MatchmakingRequestID = matchmakingRequestID

	if outcome != nil {
		o := Outcome(*outcome)
		g.Outcome = &o
	}
	if outcomeReason != nil {
		r := OutcomeReason(*outcomeReason)
		g.OutcomeReason = &r
	}

	return &g, nil
}

// CreateGame inserts a new game row. The caller is responsible for setting
// game.ID (UUID v4) before calling. The DB DEFAULT gen_random_uuid() is a
// fallback only; the application always provides an ID so the game layer can
// sign JWTs with the known ID before any DB round-trip completes.
//
// Only the columns that are meaningful at creation time are written:
// status defaults to WAITING_FOR_PLAYER, clocks default to 600000ms,
// player_black_id/outcome/outcome_reason remain NULL.
func (s *GameStore) CreateGame(ctx context.Context, game *Game) error {
	const q = `
		INSERT INTO games (id, player_white_id, current_fen, white_time_ms, black_time_ms)
		VALUES ($1, $2, $3, $4, $5)`

	_, err := s.pool.Exec(ctx, q,
		game.ID,
		game.PlayerWhiteID,
		game.CurrentFEN,
		game.WhiteTimeMs,
		game.BlackTimeMs,
	)
	if err != nil {
		return fmt.Errorf("GameStore.CreateGame gameID=%s: %w", game.ID, err)
	}
	return nil
}

// GetGame returns the game with the given ID.
// Returns ErrGameNotFound if no game exists with that ID.
func (s *GameStore) GetGame(ctx context.Context, id string) (*Game, error) {
	const q = `
		SELECT id, status, player_white_id, player_black_id,
		       current_fen, white_time_ms, black_time_ms,
		       outcome, outcome_reason,
		       white_disconnected_at, black_disconnected_at,
		       matchmaking_request_id,
		       created_at, updated_at
		FROM games
		WHERE id = $1`

	game, err := scanGame(s.pool.QueryRow(ctx, q, id).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGameNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GameStore.GetGame id=%s: %w", id, err)
	}
	return game, nil
}

// UpdateGameStatus performs an atomic conditional transition: the row's status
// is only changed from fromStatus to status if the row's current status still
// equals fromStatus at the moment PostgreSQL evaluates the WHERE clause. This
// is a compare-and-swap, not an unconditional write — the original version of
// this method had no status predicate at all, which was found during the
// Phase 2 design audit to contradict the safe-concurrent-terminal-write
// assumptions the Phase 2 walkthrough had been built on (see
// DECISIONS_LOG_PHASE_2.md ADR-021). This fix is independent of Phase 2's
// architecture and closes a real gap even under Phase 1's single-instance
// model — e.g. two goroutines racing a resign against a timeout.
//
// When the game is transitioning to a terminal state (COMPLETED or ABANDONED),
// pass a non-nil outcome carrying both the outcome and reason. For non-terminal
// transitions (WAITING → ACTIVE), pass nil — outcome and outcome_reason in the
// DB remain NULL.
//
// Every current call site already knows the row exists — it was just read from
// the DB (GetGame/GetActiveGames) or is backing an in-memory GameSession that
// was itself hydrated from a DB row — so RowsAffected() == 0 here always means
// the predicate failed (status no longer equals fromStatus), never a missing
// row. Mirrors the reasoning already established for
// UpdatePlayerBlack/ErrGameNotJoinable (ADR-016): the atomic write, not a
// prior application-level check, is the actual correctness guarantee.
func (s *GameStore) UpdateGameStatus(ctx context.Context, id string, fromStatus, status GameStatus, outcome *GameOutcome) error {
	var outcomeVal, outcomeReasonVal *string
	if outcome != nil {
		o := string(outcome.Outcome)
		r := string(outcome.Reason)
		outcomeVal = &o
		outcomeReasonVal = &r
	}

	const q = `
		UPDATE games
		SET status = $1, outcome = $2, outcome_reason = $3, updated_at = NOW()
		WHERE id = $4 AND status = $5`

	tag, err := s.pool.Exec(ctx, q, string(status), outcomeVal, outcomeReasonVal, id, string(fromStatus))
	if err != nil {
		return fmt.Errorf("GameStore.UpdateGameStatus gameID=%s fromStatus=%s status=%s: %w", id, fromStatus, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdateGameStatus gameID=%s fromStatus=%s status=%s: %w", id, fromStatus, status, ErrGameStatusConflict)
	}
	return nil
}

// UpdateCurrentFEN sets the current board position on the game record.
// Called after every successfully persisted move.
func (s *GameStore) UpdateCurrentFEN(ctx context.Context, id string, fen string) error {
	const q = `UPDATE games SET current_fen = $1, updated_at = NOW() WHERE id = $2`

	tag, err := s.pool.Exec(ctx, q, fen, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdateCurrentFEN gameID=%s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdateCurrentFEN gameID=%s: %w", id, ErrGameNotFound)
	}
	return nil
}

// UpdatePlayerBlack sets player_black_id when the second player joins via
// POST /games/:id/join. Called once per game lifetime.
//
// The UPDATE's WHERE clause (status = WAITING_FOR_PLAYER AND player_black_id
// IS NULL) makes this an atomic conditional write, not a check-then-act pair
// with the caller's prior GetGame read. Two concurrent JoinGame calls for the
// same gameID can both pass a pre-flight GetGame check (both observe
// player_black_id IS NULL) before either commits — only this UPDATE's
// predicate, evaluated atomically by PostgreSQL, decides the actual winner.
// See ADR-016 in DECISIONS_LOG_PHASE_1.md.
func (s *GameStore) UpdatePlayerBlack(ctx context.Context, id string, playerBlackID string) error {
	const q = `UPDATE games SET player_black_id = $1, updated_at = NOW() WHERE id = $2 AND status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL`

	tag, err := s.pool.Exec(ctx, q, playerBlackID, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdatePlayerBlack gameID=%s playerBlackID=%s: %w", id, playerBlackID, err)
	}
	if tag.RowsAffected() == 0 {
		// Zero rows affected means the row exists but the predicate failed —
		// game already has a Black player, or is no longer WAITING_FOR_PLAYER.
		// This is NOT "not found": the caller already confirmed existence via
		// GetGame before calling. Returning ErrGameNotFound here would be a
		// misleading error for the loser of a genuine join race.
		return fmt.Errorf("GameStore.UpdatePlayerBlack gameID=%s: %w", id, ErrGameNotJoinable)
	}
	return nil
}

// CreateMatchedGame atomically inserts a new game row with both players
// already assigned, for matchmaking-originated games
// (DECISIONS_LOG_PHASE_3.md ADR-032/ADR-034). Unlike CreateGame (which
// inserts a single-player WAITING_FOR_PLAYER row, followed later by a
// separate UpdatePlayerBlack call for the shared-link join flow), both
// players are already known at pairing time, so this is one INSERT, not a
// create-then-join pair.
//
// Status is deliberately left unset here and takes the same DB DEFAULT
// ('WAITING_FOR_PLAYER') CreateGame relies on. Both players being assigned
// in the row from the start does NOT mean the game is ACTIVE: that
// transition still only happens when both players actually connect over
// WebSocket (Manager.HandleConnect), exactly as for a shared-link game.
// This is confirmed load-bearing by TD-P3-004 (PHASE_3.md): a matched game
// whose assigned opponent never connects sits in WAITING_FOR_PLAYER, not
// ACTIVE — which is only true if this method leaves status at its default
// rather than setting it to ACTIVE at insert time.
//
// game.MatchmakingRequestID must be set by the caller: one UUID v4
// generated per ZPOPMIN-won pairing attempt (chess-server's pairing loop,
// Phase 3 Step 3 — not yet implemented), reused across every retry of that
// same attempt. The INSERT is issued with
// ON CONFLICT (matchmaking_request_id) DO NOTHING, making this method safe
// to call again with the same requestID after an ambiguous failure
// (timeout / lost ACK) without risking a duplicate game or a double-booked
// pair (ADR-034's rationale, corrected by ADR-039): if a prior call with the
// same requestID already committed, this call becomes a no-op rather than a
// second insert attempt or a UNIQUE-violation error.
//
// inserted reports whether THIS call performed the insert (true) or found a
// row already committed under the same MatchmakingRequestID from a prior
// call (false — RowsAffected()==0, the idempotent-retry case ADR-034 exists
// to make safe). Both are success outcomes from this method's perspective —
// deciding what "inserted == false" means for retry/report logic belongs to
// the pairing-loop orchestration layer (Phase 3 Step 3), not the store
// layer. This method deliberately does not fetch the existing row on a
// no-op; the caller knows why it's retrying and what it needs next, this
// method doesn't guess on its behalf.
func (s *GameStore) CreateMatchedGame(ctx context.Context, game *Game) (inserted bool, err error) {
	if game.PlayerBlackID == nil {
		return false, fmt.Errorf("GameStore.CreateMatchedGame gameID=%s: player_black_id must be set for a matched game", game.ID)
	}
	if game.MatchmakingRequestID == nil {
		return false, fmt.Errorf("GameStore.CreateMatchedGame gameID=%s: matchmaking_request_id must be set", game.ID)
	}

	const q = `
		INSERT INTO games (id, player_white_id, player_black_id, current_fen, white_time_ms, black_time_ms, matchmaking_request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (matchmaking_request_id) DO NOTHING`

	tag, err := s.pool.Exec(ctx, q,
		game.ID,
		game.PlayerWhiteID,
		game.PlayerBlackID,
		game.CurrentFEN,
		game.WhiteTimeMs,
		game.BlackTimeMs,
		game.MatchmakingRequestID,
	)
	if err != nil {
		return false, fmt.Errorf("GameStore.CreateMatchedGame gameID=%s requestID=%s: %w", game.ID, *game.MatchmakingRequestID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetActiveGames returns all games in WAITING_FOR_PLAYER or ACTIVE status.
// Used on server restart to hydrate the in-memory GameRegistry from persisted state.
func (s *GameStore) GetActiveGames(ctx context.Context) ([]*Game, error) {
	const q = `
		SELECT id, status, player_white_id, player_black_id,
		       current_fen, white_time_ms, black_time_ms,
		       outcome, outcome_reason,
		       white_disconnected_at, black_disconnected_at,
		       matchmaking_request_id,
		       created_at, updated_at
		FROM games
		WHERE status IN ('WAITING_FOR_PLAYER', 'ACTIVE')`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("GameStore.GetActiveGames: %w", err)
	}
	defer rows.Close()

	games := make([]*Game, 0)
	for rows.Next() {
		game, err := scanGame(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("GameStore.GetActiveGames scan: %w", err)
		}
		games = append(games, game)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GameStore.GetActiveGames rows: %w", err)
	}

	return games, nil
}

// UpdateClocks persists both players' remaining time.
// Called after every move and on player disconnect so that a server restart
// can resume clocks from the last known values.
func (s *GameStore) UpdateClocks(ctx context.Context, id string, whiteMs, blackMs int64) error {
	const q = `
		UPDATE games
		SET white_time_ms = $1, black_time_ms = $2, updated_at = NOW()
		WHERE id = $3`

	tag, err := s.pool.Exec(ctx, q, whiteMs, blackMs, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdateClocks gameID=%s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdateClocks gameID=%s: %w", id, ErrGameNotFound)
	}
	return nil
}

// UpdateDisconnectTimestamp sets or clears the disconnect-grace-period
// timestamp for the given color (DECISIONS_LOG_PHASE_2.md ADR-030). Passing
// a non-nil t records "this player disconnected at t"; passing nil clears it
// (reconnect). This is what lets a surviving instance, after a failover,
// resume or immediately resolve an abandonment grace period it has no
// in-memory record of — Manager.abandonTimers is pure per-process state and
// does not survive the owning process dying.
func (s *GameStore) UpdateDisconnectTimestamp(ctx context.Context, id string, color Color, t *time.Time) error {
	var q string
	switch color {
	case ColorWhite:
		q = `UPDATE games SET white_disconnected_at = $1, updated_at = NOW() WHERE id = $2`
	case ColorBlack:
		q = `UPDATE games SET black_disconnected_at = $1, updated_at = NOW() WHERE id = $2`
	default:
		return fmt.Errorf("GameStore.UpdateDisconnectTimestamp gameID=%s: invalid color %q", id, color)
	}

	tag, err := s.pool.Exec(ctx, q, t, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdateDisconnectTimestamp gameID=%s color=%s: %w", id, color, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdateDisconnectTimestamp gameID=%s: %w", id, ErrGameNotFound)
	}
	return nil
}
