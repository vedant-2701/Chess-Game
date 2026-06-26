// Package chess wraps the notnil/chess library. It is the only package in this
// codebase that imports notnil/chess directly. All other packages that need chess
// operations go through this package.
//
// The chess layer owns four concerns:
//   - Move validation (without mutation)
//   - Move application (mutation, called only after DB write succeeds)
//   - Game state interrogation (FEN, move history, turn)
//   - Outcome detection (checkmate, stalemate)
//
// This separation exists because of the persistence-first guarantee: a move must be
// persisted to PostgreSQL before the in-memory board state advances. ValidateMove and
// ApplyMove are separate functions so the move pipeline can place the DB write between
// them. See ADR-013.
package chess

import (
	"fmt"

	"github.com/notnil/chess"
)

// Validator wraps notnil/chess and provides the chess operations needed by the
// move pipeline and game session. It holds no state; all game state lives in
// *chess.Game values owned by GameSession.
type Validator struct{}

// NewValidator returns a new Validator. It cannot fail.
func NewValidator() *Validator {
	return &Validator{}
}

// NewGame returns a *chess.Game at the standard starting position with
// AlgebraicNotation (SAN) set as the default notation. This is the notation
// used throughout the WebSocket protocol.
func NewGame() *chess.Game {
	return chess.NewGame(chess.UseNotation(chess.AlgebraicNotation{}))
}

// GameFromFEN reconstructs a *chess.Game from a FEN string. The returned game
// has a single position in its history (the position encoded by the FEN). This
// is used for server restart recovery where a game is resumed from its persisted
// current_fen. Note: position history prior to the FEN snapshot is not recoverable
// from FEN alone; threefold repetition detection will not reflect moves played
// before the server restart. This is an accepted limitation documented as a
// known risk in PHASE_1.md.
func GameFromFEN(fen string) (*chess.Game, error) {
	fenOpt, err := chess.FEN(fen)
	if err != nil {
		return nil, fmt.Errorf("GameFromFEN: %w: %v", ErrInvalidFEN, err)
	}
	g := chess.NewGame(fenOpt, chess.UseNotation(chess.AlgebraicNotation{}))
	return g, nil
}

// GameFromMoves replays a slice of SAN move strings onto a new game starting
// from the initial position. This preserves full position history including all
// prior positions, making threefold repetition detection accurate. Used for
// hydrating GameSession from the moves table on reconnection or server restart
// when full history accuracy is required.
func GameFromMoves(moves []string) (*chess.Game, error) {
	g := chess.NewGame(chess.UseNotation(chess.AlgebraicNotation{}))
	for i, san := range moves {
		if err := g.MoveStr(san); err != nil {
			return nil, fmt.Errorf("GameFromMoves: move %d %q: %w", i+1, san, ErrIllegalMove)
		}
	}
	return g, nil
}

// ValidateMove checks whether san is a legal move in the current position of g.
// It does not mutate g. Returns ErrIllegalMove if the move is not legal.
//
// This is the first half of the validate-then-apply split (ADR-013). It must be
// called before the DB write. ApplyMove must be called after the DB write succeeds.
func (v *Validator) ValidateMove(g *chess.Game, san string) error {
	// AlgebraicNotation.Decode validates syntax and legality against the current
	// position's valid move list. It returns an error for both malformed SAN and
	// moves that are syntactically valid but illegal in the current position.
	_, err := chess.AlgebraicNotation{}.Decode(g.Position(), san)
	if err != nil {
		return fmt.Errorf("ValidateMove %q: %w", san, ErrIllegalMove)
	}
	return nil
}

// ApplyMove applies san to g in-place. It must only be called after ValidateMove
// returned nil AND after the move has been successfully persisted to the database.
//
// ApplyMove returning an error after ValidateMove returned nil for the same (g, san)
// pair is a bug — it means the game state changed between validation and application,
// which violates the single-goroutine-per-session invariant. If this happens, the
// error must be logged as an error-level event with full context; the game state is
// unrecoverable without reloading from the database.
func (v *Validator) ApplyMove(g *chess.Game, san string) error {
	if err := g.MoveStr(san); err != nil {
		return fmt.Errorf("ApplyMove %q: %w", san, err)
	}
	return nil
}

// DetectOutcome checks whether the game has ended after the most recent move.
// Returns (outcome, true) if the game is over, or (zero value, false) if play
// continues. Only detects outcomes that the chess engine can determine: checkmate
// and stalemate. Resignation, timeout, and abandonment are handled by the game
// layer and never returned here.
//
// DetectOutcome must be called after ApplyMove, not before. Calling it before
// applying a move checks the outcome of the previous position.
func (v *Validator) DetectOutcome(g *chess.Game) (*GameOutcome, bool) {
	outcome := g.Outcome()
	method := g.Method()

	// notnil/chess sets outcome automatically after each MoveStr call when the
	// game ends. NoOutcome ("*") means the game is still in progress.
	if outcome == chess.NoOutcome {
		return nil, false
	}

	result := &GameOutcome{}

	switch outcome {
	case chess.WhiteWon:
		result.Winner = "WHITE"
	case chess.BlackWon:
		result.Winner = "BLACK"
	case chess.Draw:
		result.Winner = "DRAW"
	}

	switch method {
	case chess.Checkmate:
		result.Reason = "CHECKMATE"
	case chess.Stalemate:
		result.Reason = "STALEMATE"
	default:
		// ThreefoldRepetition, FiftyMoveRule, and InsufficientMaterial are auto-detected
		// by notnil/chess. The store schema allows only the values defined in
		// OutcomeReason. DRAW_AGREEMENT is the closest valid reason for auto-draws
		// that are not stalemate. Phase 4 will add distinct reasons to the schema.
		result.Reason = "DRAW_AGREEMENT"
	}

	return result, true
}

// CurrentFEN returns the FEN string of the current position in g. The returned
// string is suitable for storage in games.current_fen and for sending in
// GAME_STATE and MOVE_APPLIED WebSocket messages.
func CurrentFEN(g *chess.Game) string {
	return g.FEN()
}

// MoveHistory returns the ordered list of moves played in g as SAN strings.
// The slice is empty (not nil) if no moves have been played. It is suitable
// for inclusion in GAME_STATE messages as the "moves" field.
//
// Implementation note: notnil/chess v1.9.0 panics in game.MoveHistory() when
// the internal comments slice is nil, which occurs for games constructed via
// NewGame() + MoveStr() without PGN parsing. We use g.Moves() and g.Positions()
// directly to avoid this panic.
func MoveHistory(g *chess.Game) []string {
	moves := g.Moves()
	positions := g.Positions()
	sans := make([]string, 0, len(moves))
	notation := chess.AlgebraicNotation{}
	for i, m := range moves {
		if i >= len(positions) {
			break
		}
		san := notation.Encode(positions[i], m)
		sans = append(sans, san)
	}
	return sans
}