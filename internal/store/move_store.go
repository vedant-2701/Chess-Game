package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MoveStore handles persistence for the moves table.
type MoveStore struct {
	pool *pgxpool.Pool
}

// NewMoveStore constructs a MoveStore backed by the given pool.
func NewMoveStore(pool *pgxpool.Pool) *MoveStore {
	return &MoveStore{pool: pool}
}

// SaveMove inserts a move record and populates move.ID and move.PlayedAt from
// the database RETURNING clause. The caller must set all other fields before calling.
//
// The UNIQUE index on (game_id, move_number) enforces sequencing integrity at
// the database level. A duplicate move_number for the same game will return an
// error — the move pipeline (Step 8) must handle this as a concurrency failure.
func (s *MoveStore) SaveMove(ctx context.Context, move *Move) error {
	const q = `
		INSERT INTO moves (game_id, move_number, color, san, fen_after)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, played_at`

	err := s.pool.QueryRow(ctx, q,
		move.GameID,
		move.MoveNumber,
		string(move.Color),
		move.SAN,
		move.FENAfter,
	).Scan(&move.ID, &move.PlayedAt)
	if err != nil {
		return fmt.Errorf("MoveStore.SaveMove gameID=%s moveNumber=%d san=%s: %w",
			move.GameID, move.MoveNumber, move.SAN, err)
	}
	return nil
}

// GetMovesForGame returns all moves for a game in ascending move_number order.
// Returns an empty (non-nil) slice if the game has no moves yet.
// Used for reconnection state delivery and server restart recovery.
func (s *MoveStore) GetMovesForGame(ctx context.Context, gameID string) ([]*Move, error) {
	const q = `
		SELECT id, game_id, move_number, color, san, fen_after, played_at
		FROM moves
		WHERE game_id = $1
		ORDER BY move_number ASC`

	rows, err := s.pool.Query(ctx, q, gameID)
	if err != nil {
		return nil, fmt.Errorf("MoveStore.GetMovesForGame gameID=%s: %w", gameID, err)
	}
	defer rows.Close()

	moves := make([]*Move, 0)
	for rows.Next() {
		var m Move
		var colorStr string

		err := rows.Scan(
			&m.ID,
			&m.GameID,
			&m.MoveNumber,
			&colorStr,
			&m.SAN,
			&m.FENAfter,
			&m.PlayedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("MoveStore.GetMovesForGame gameID=%s scan: %w", gameID, err)
		}
		m.Color = Color(colorStr)
		moves = append(moves, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("MoveStore.GetMovesForGame gameID=%s rows: %w", gameID, err)
	}

	return moves, nil
}