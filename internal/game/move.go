package game

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

// MoveProcessor executes the full move pipeline for a game session.
// It is the only component that advances the in-memory board state, and it does
// so only after the move is confirmed in PostgreSQL (persistence-first invariant,
// ADR-013).
type MoveProcessor struct {
	validator *internalchess.Validator
	gameStore *store.GameStore
	moveStore *store.MoveStore
	eventBus  EventBus
}

// NewMoveProcessor constructs a MoveProcessor with its required dependencies.
func NewMoveProcessor(
	validator *internalchess.Validator,
	gameStore *store.GameStore,
	moveStore *store.MoveStore,
	eventBus EventBus,
) *MoveProcessor {
	return &MoveProcessor{
		validator: validator,
		gameStore: gameStore,
		moveStore: moveStore,
		eventBus:  eventBus,
	}
}

// moveAppliedMsg is the JSON payload for MOVE_APPLIED WebSocket messages.
// Field names are camelCase per the WebSocket protocol spec in PHASE_1.md.
type moveAppliedMsg struct {
	Type        string `json:"type"`
	SAN         string `json:"san"`
	FEN         string `json:"fen"`
	Turn        string `json:"turn"`
	MoveNumber  int    `json:"moveNumber"`
	WhiteTimeMs int64  `json:"whiteTimeMs"`
	BlackTimeMs int64  `json:"blackTimeMs"`
}

// gameOverMsg is the JSON payload for GAME_OVER WebSocket messages.
type gameOverMsg struct {
	Type    string `json:"type"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	FEN     string `json:"fen"`
}

// ProcessMove executes the full move pipeline for a submitted player move.
// The pipeline sequence is strictly ordered per ADR-013 and the
// persistence-first invariant:
//
//  1. Check game is ACTIVE and it is color's turn
//  2. Validate move legality under session.mu.RLock (no board mutation)
//  3. Compute FEN after the move under session.mu.RLock (no board mutation)
//  4. Persist the move to the database
//  5. Update current_fen on the game record
//  6. Apply the move to the in-memory board under session.mu.Lock
//  7. Detect game outcome
//  8. Publish MOVE_APPLIED or GAME_OVER via EventBus
//
// Returns *MoveRejectionError for client-attributable rejections (wrong turn,
// illegal move, game not active). Returns a plain error for infrastructure
// failures (DB errors, unexpected internal states). The Manager uses errors.As
// to distinguish and send the appropriate WebSocket message.
func (p *MoveProcessor) ProcessMove(ctx context.Context, session *GameSession, color store.Color, san string) error {
	// Step 1: status and turn check from a consistent snapshot.
	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive {
		return &MoveRejectionError{Reason: RejectReasonGameNotActive}
	}
	if snap.Turn != color {
		return &MoveRejectionError{Reason: RejectReasonNotYourTurn}
	}

	// Steps 2–3: validate legality and compute fenAfter under the read lock.
	// session.board is protected by session.mu per the GameSession invariant.
	// A single RLock acquisition covers both operations so the position cannot
	// change between them, and avoids two separate lock round-trips.
	var fenAfter string
	var rejErr *MoveRejectionError

	session.mu.RLock()
	if err := p.validator.ValidateMove(session.board, san); err != nil {
		rejErr = &MoveRejectionError{Reason: RejectReasonIllegalMove}
	} else {
		var computeErr error
		fenAfter, computeErr = internalchess.ComputeFENAfterMove(session.board, san)
		if computeErr != nil {
			// ValidateMove and ComputeFENAfterMove use the same Decode path.
			// ValidateMove passed, so arriving here is a bug.
			session.mu.RUnlock()
			slog.Error("ComputeFENAfterMove failed after ValidateMove succeeded — this is a bug",
				"gameID", session.ID, "san", san, "error", computeErr)
			return fmt.Errorf("ProcessMove gameID=%s san=%s: compute fen after: %w",
				session.ID, san, computeErr)
		}
	}
	session.mu.RUnlock()

	if rejErr != nil {
		return rejErr
	}

	// Step 4: persist the move. Board state must not advance before this succeeds.
	move := &store.Move{
		GameID:     session.ID,
		MoveNumber: len(snap.Moves) + 1,
		Color:      color,
		SAN:        san,
		FENAfter:   fenAfter,
	}
	if err := p.moveStore.SaveMove(ctx, move); err != nil {
		slog.Error("failed to save move",
			"gameID", session.ID, "san", san, "moveNumber", move.MoveNumber, "error", err)
		return fmt.Errorf("ProcessMove gameID=%s san=%s moveNumber=%d: save move: %w",
			session.ID, san, move.MoveNumber, err)
	}

	// Step 5: update current_fen on the game record.
	// If this fails after SaveMove succeeded, current_fen is stale but the move
	// is safely recorded in the moves table. current_fen is a denormalized cache;
	// the moves table is the source of truth. Log the error and continue.
	if err := p.gameStore.UpdateCurrentFEN(ctx, session.ID, fenAfter); err != nil {
		slog.Error("failed to update current_fen after move saved — current_fen is stale",
			"gameID", session.ID, "san", san, "error", err)
	}

	// Step 6: apply the move to the in-memory board under the write lock.
	// session.mu.Lock prevents a data race with CurrentStateSnapshot or any other
	// concurrent reader of session.board.
	session.mu.Lock()
	applyErr := p.validator.ApplyMove(session.board, san)
	session.mu.Unlock()

	if applyErr != nil {
		// ValidateMove passed for the same (board, san) pair. ApplyMove failing
		// means the board changed between validation and application — a violation
		// of the single-goroutine-per-session invariant. The game is unrecoverable
		// without reloading state from the database.
		slog.Error("ApplyMove failed after ValidateMove succeeded — game state unrecoverable",
			"gameID", session.ID, "san", san, "error", applyErr)
		return fmt.Errorf("ProcessMove gameID=%s san=%s: apply move: %w",
			session.ID, san, applyErr)
	}

	// Step 7: detect outcome after the move is applied.
	// DetectOutcome reads session.board; no lock needed — the write is complete
	// and only one goroutine calls ProcessMove per session at a time.
	if outcome, ended := p.validator.DetectOutcome(session.board); ended {
		return p.handleGameOver(ctx, session, fenAfter, outcome)
	}

	// Step 8: no outcome — publish MOVE_APPLIED.
	return p.publishMoveApplied(ctx, session, san, fenAfter, move.MoveNumber, color, snap)
}

// handleGameOver transitions the session to COMPLETED, persists the outcome to
// the database, and publishes a GAME_OVER event via the EventBus.
//
// If the DB update fails after the in-memory transition, the error is logged and
// the GAME_OVER event is still published — players must be notified regardless of
// DB consistency.
func (p *MoveProcessor) handleGameOver(
	ctx context.Context,
	session *GameSession,
	fenAfter string,
	outcome *internalchess.GameOutcome,
) error {
	if err := session.Transition(store.GameStatusCompleted); err != nil {
		slog.Error("failed to transition game to COMPLETED",
			"gameID", session.ID, "outcome", outcome.Winner, "error", err)
		return fmt.Errorf("ProcessMove.handleGameOver gameID=%s: transition: %w",
			session.ID, err)
	}

	storeOutcome := store.Outcome(outcome.Winner)
	storeReason := store.OutcomeReason(outcome.Reason)
	session.SetOutcome(storeOutcome, storeReason)

	if err := p.gameStore.UpdateGameStatus(ctx, session.ID, store.GameStatusCompleted, &store.GameOutcome{
		Outcome: storeOutcome,
		Reason:  storeReason,
	}); err != nil {
		slog.Error("failed to persist COMPLETED status",
			"gameID", session.ID, "outcome", outcome.Winner, "reason", outcome.Reason, "error", err)
		// Continue: players must still receive GAME_OVER.
	}

	msg := gameOverMsg{
		Type:    MsgTypeGameOver,
		Outcome: outcome.Winner,
		Reason:  outcome.Reason,
		FEN:     fenAfter,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ProcessMove.handleGameOver gameID=%s: marshal: %w",
			session.ID, err)
	}

	if err := p.eventBus.Publish(ctx, GameEvent{
		GameID:  session.ID,
		Type:    MsgTypeGameOver,
		Payload: payload,
	}); err != nil {
		slog.Error("failed to publish GAME_OVER event",
			"gameID", session.ID, "error", err)
	}
	return nil
}

// publishMoveApplied marshals and publishes a MOVE_APPLIED event via the EventBus.
// Clock values come from the pre-move snapshot; clock switching is wired in at
// Step 9 when the Clock is implemented.
func (p *MoveProcessor) publishMoveApplied(
	ctx context.Context,
	session *GameSession,
	san, fenAfter string,
	moveNumber int,
	color store.Color,
	snap GameStateSnapshot,
) error {
	nextTurn := store.ColorBlack
	if color == store.ColorBlack {
		nextTurn = store.ColorWhite
	}

	msg := moveAppliedMsg{
		Type:        MsgTypeMoveApplied,
		SAN:         san,
		FEN:         fenAfter,
		Turn:        string(nextTurn),
		MoveNumber:  moveNumber,
		WhiteTimeMs: snap.WhiteTimeMs,
		BlackTimeMs: snap.BlackTimeMs,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ProcessMove.publishMoveApplied gameID=%s san=%s: marshal: %w",
			session.ID, san, err)
	}

	if err := p.eventBus.Publish(ctx, GameEvent{
		GameID:  session.ID,
		Type:    MsgTypeMoveApplied,
		Payload: payload,
	}); err != nil {
		slog.Error("failed to publish MOVE_APPLIED event",
			"gameID", session.ID, "san", san, "error", err)
	}
	return nil
}
