package store

import (
	"context"
	"errors"
	"fmt"

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
		g              Game
		statusStr      string
		playerBlackID  *string
		outcome        *string
		outcomeReason  *string
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
		&g.CreatedAt,
		&g.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	g.Status = GameStatus(statusStr)
	g.PlayerBlackID = playerBlackID

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
		       outcome, outcome_reason, created_at, updated_at
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

// UpdateGameStatus sets the game's status. When the game is transitioning to a
// terminal state (COMPLETED or ABANDONED), pass a non-nil outcome carrying both
// the outcome and reason. For non-terminal transitions (e.g. WAITING → ACTIVE),
// pass nil — outcome and outcome_reason in the DB remain NULL.
func (s *GameStore) UpdateGameStatus(ctx context.Context, id string, status GameStatus, outcome *GameOutcome) error {
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
		WHERE id = $4`

	tag, err := s.pool.Exec(ctx, q, string(status), outcomeVal, outcomeReasonVal, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdateGameStatus gameID=%s status=%s: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdateGameStatus gameID=%s: %w", id, ErrGameNotFound)
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
func (s *GameStore) UpdatePlayerBlack(ctx context.Context, id string, playerBlackID string) error {
	const q = `UPDATE games SET player_black_id = $1, updated_at = NOW() WHERE id = $2`

	tag, err := s.pool.Exec(ctx, q, playerBlackID, id)
	if err != nil {
		return fmt.Errorf("GameStore.UpdatePlayerBlack gameID=%s playerBlackID=%s: %w", id, playerBlackID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("GameStore.UpdatePlayerBlack gameID=%s: %w", id, ErrGameNotFound)
	}
	return nil
}

// GetActiveGames returns all games in WAITING_FOR_PLAYER or ACTIVE status.
// Used on server restart to hydrate the in-memory GameRegistry from persisted state.
func (s *GameStore) GetActiveGames(ctx context.Context) ([]*Game, error) {
	const q = `
		SELECT id, status, player_white_id, player_black_id,
		       current_fen, white_time_ms, black_time_ms,
		       outcome, outcome_reason, created_at, updated_at
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