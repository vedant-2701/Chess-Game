//go:build integration

package game

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

const (
	moveTestWhiteID = "00000000-0000-0000-0000-000000000001"
	moveTestBlackID = "00000000-0000-0000-0000-000000000002"
	moveTestGameID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

// newMoveProcessor builds a MoveProcessor wired to testPool.
func newMoveProcessor(bus EventBus) *MoveProcessor {
	return NewMoveProcessor(
		internalchess.NewValidator(),
		store.NewGameStore(testPool),
		store.NewMoveStore(testPool),
		bus,
	)
}

func TestMoveProcessor_ValidMove_FullPipeline(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	ctx := context.Background()
	bus := NewLocalEventBus()
	session := mustCreateActiveGame(t, moveTestGameID, moveTestWhiteID, moveTestBlackID)
	processor := newMoveProcessor(bus)

	ch, unsubscribe, err := bus.Subscribe(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsubscribe()

	if err := processor.ProcessMove(ctx, session, store.ColorWhite, "e4"); err != nil {
		t.Fatalf("ProcessMove: %v", err)
	}

	// Verify MOVE_APPLIED event was published.
	select {
	case event := <-ch:
		if event.Type != MsgTypeMoveApplied {
			t.Errorf("event.Type: got %q, want %q", event.Type, MsgTypeMoveApplied)
		}
		var payload moveAppliedMsg
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.SAN != "e4" {
			t.Errorf("payload.SAN: got %q, want %q", payload.SAN, "e4")
		}
		if payload.Turn != string(store.ColorBlack) {
			t.Errorf("payload.Turn: got %q, want %q", payload.Turn, store.ColorBlack)
		}
		if payload.MoveNumber != 1 {
			t.Errorf("payload.MoveNumber: got %d, want 1", payload.MoveNumber)
		}
	case <-time.After(time.Second):
		t.Fatal("no MOVE_APPLIED event received within 1s")
	}

	// Verify DB state: move persisted.
	moves, err := store.NewMoveStore(testPool).GetMovesForGame(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("GetMovesForGame: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("moves in DB: got %d, want 1", len(moves))
	}
	if moves[0].SAN != "e4" {
		t.Errorf("move[0].SAN: got %q, want %q", moves[0].SAN, "e4")
	}
	if moves[0].Color != store.ColorWhite {
		t.Errorf("move[0].Color: got %q, want %q", moves[0].Color, store.ColorWhite)
	}
	if moves[0].MoveNumber != 1 {
		t.Errorf("move[0].MoveNumber: got %d, want 1", moves[0].MoveNumber)
	}
	if moves[0].FENAfter == store.StartingFEN {
		t.Error("move[0].FENAfter should differ from StartingFEN after e4")
	}

	// Verify DB state: current_fen updated.
	game, err := store.NewGameStore(testPool).GetGame(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.CurrentFEN == store.StartingFEN {
		t.Error("game.CurrentFEN should differ from StartingFEN after e4")
	}
	if game.CurrentFEN != moves[0].FENAfter {
		t.Errorf("game.CurrentFEN != move FENAfter: %q vs %q", game.CurrentFEN, moves[0].FENAfter)
	}

	// Verify in-memory board state advanced.
	snap := session.CurrentStateSnapshot()
	if snap.Turn != store.ColorBlack {
		t.Errorf("snap.Turn: got %q, want BLACK", snap.Turn)
	}
	if len(snap.Moves) != 1 {
		t.Errorf("snap.Moves: got %d moves, want 1", len(snap.Moves))
	}
	if snap.CurrentFEN == store.StartingFEN {
		t.Error("snap.CurrentFEN should differ from StartingFEN after e4")
	}
}

func TestMoveProcessor_WrongTurn_RejectsWithMoveRejectionError(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	ctx := context.Background()
	bus := NewLocalEventBus()
	session := mustCreateActiveGame(t, moveTestGameID, moveTestWhiteID, moveTestBlackID)
	processor := newMoveProcessor(bus)

	// It is White's turn; Black attempts to move.
	err := processor.ProcessMove(ctx, session, store.ColorBlack, "e5")
	if err == nil {
		t.Fatal("expected error for wrong turn, got nil")
	}

	var rejection *MoveRejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("expected *MoveRejectionError, got: %T %v", err, err)
	}
	if rejection.Reason != RejectReasonNotYourTurn {
		t.Errorf("rejection.Reason: got %q, want %q", rejection.Reason, RejectReasonNotYourTurn)
	}

	// Board must be unchanged.
	snap := session.CurrentStateSnapshot()
	if snap.CurrentFEN != store.StartingFEN {
		t.Errorf("board changed after wrong-turn rejection: %q", snap.CurrentFEN)
	}
	if len(snap.Moves) != 0 {
		t.Errorf("snap.Moves: got %d, want 0", len(snap.Moves))
	}

	// Nothing persisted.
	moves, err := store.NewMoveStore(testPool).GetMovesForGame(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("GetMovesForGame: %v", err)
	}
	if len(moves) != 0 {
		t.Errorf("moves in DB: got %d, want 0", len(moves))
	}
}

func TestMoveProcessor_IllegalMove_RejectsAndLeavesBoard(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	ctx := context.Background()
	bus := NewLocalEventBus()
	session := mustCreateActiveGame(t, moveTestGameID, moveTestWhiteID, moveTestBlackID)
	processor := newMoveProcessor(bus)

	// e5 is Black's pawn move — illegal when it is White's turn (and also
	// structurally illegal from White's perspective in starting position).
	err := processor.ProcessMove(ctx, session, store.ColorWhite, "e5")
	if err == nil {
		t.Fatal("expected error for illegal move, got nil")
	}

	var rejection *MoveRejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("expected *MoveRejectionError, got: %T %v", err, err)
	}
	if rejection.Reason != RejectReasonIllegalMove {
		t.Errorf("rejection.Reason: got %q, want %q", rejection.Reason, RejectReasonIllegalMove)
	}

	// Board must be unchanged.
	snap := session.CurrentStateSnapshot()
	if snap.CurrentFEN != store.StartingFEN {
		t.Errorf("board changed after illegal move rejection: %q", snap.CurrentFEN)
	}

	// Nothing persisted.
	moves, err := store.NewMoveStore(testPool).GetMovesForGame(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("GetMovesForGame: %v", err)
	}
	if len(moves) != 0 {
		t.Errorf("moves in DB after illegal move: got %d, want 0", len(moves))
	}
}

func TestMoveProcessor_DBFailure_LeavesBoard(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	bus := NewLocalEventBus()
	session := mustCreateActiveGame(t, moveTestGameID, moveTestWhiteID, moveTestBlackID)
	processor := newMoveProcessor(bus)

	// A cancelled context causes SaveMove to fail, simulating a DB write failure.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := processor.ProcessMove(ctx, session, store.ColorWhite, "e4")
	if err == nil {
		t.Fatal("expected error for DB failure, got nil")
	}
	// Must NOT be a MoveRejectionError — this is an infrastructure failure.
	var rejection *MoveRejectionError
	if errors.As(err, &rejection) {
		t.Fatalf("DB failure should not return *MoveRejectionError, got: %v", err)
	}

	// Board must be unchanged — persistence-first invariant holds.
	snap := session.CurrentStateSnapshot()
	if snap.CurrentFEN != store.StartingFEN {
		t.Errorf("board advanced despite DB failure: %q", snap.CurrentFEN)
	}
	if len(snap.Moves) != 0 {
		t.Errorf("snap.Moves: got %d, want 0", len(snap.Moves))
	}
}

func TestMoveProcessor_CheckmateDetected_PublishesGameOver(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	ctx := context.Background()
	bus := NewLocalEventBus()
	session := mustCreateActiveGame(t, moveTestGameID, moveTestWhiteID, moveTestBlackID)
	processor := newMoveProcessor(bus)

	ch, unsubscribe, err := bus.Subscribe(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsubscribe()

	// Scholar's mate: e4, e5, Qh5, Nc6, Bc4, Nf6, Qxf7#
	// White wins by checkmate on move 4 (7 half-moves).
	scholarsMate := []struct {
		color store.Color
		san   string
	}{
		{store.ColorWhite, "e4"},
		{store.ColorBlack, "e5"},
		{store.ColorWhite, "Qh5"},
		{store.ColorBlack, "Nc6"},
		{store.ColorWhite, "Bc4"},
		{store.ColorBlack, "Nf6"},
		{store.ColorWhite, "Qxf7"},
	}

	// Drain MOVE_APPLIED events for all but the last move.
	for i, m := range scholarsMate[:len(scholarsMate)-1] {
		if err := processor.ProcessMove(ctx, session, m.color, m.san); err != nil {
			t.Fatalf("ProcessMove move %d %q: %v", i+1, m.san, err)
		}
		select {
		case event := <-ch:
			if event.Type != MsgTypeMoveApplied {
				t.Errorf("move %d: expected MOVE_APPLIED, got %q", i+1, event.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("move %d: no event received within 1s", i+1)
		}
	}

	// Final move should produce GAME_OVER.
	last := scholarsMate[len(scholarsMate)-1]
	if err := processor.ProcessMove(ctx, session, last.color, last.san); err != nil {
		t.Fatalf("ProcessMove checkmate move %q: %v", last.san, err)
	}

	select {
	case event := <-ch:
		if event.Type != MsgTypeGameOver {
			t.Errorf("expected GAME_OVER event, got %q", event.Type)
		}
		var payload gameOverMsg
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal GAME_OVER payload: %v", err)
		}
		if payload.Outcome != "WHITE" {
			t.Errorf("payload.Outcome: got %q, want WHITE", payload.Outcome)
		}
		if payload.Reason != "CHECKMATE" {
			t.Errorf("payload.Reason: got %q, want CHECKMATE", payload.Reason)
		}
		if payload.FEN == "" {
			t.Error("payload.FEN must not be empty")
		}
	case <-time.After(time.Second):
		t.Fatal("no GAME_OVER event received within 1s")
	}

	// Verify session transitioned to COMPLETED in memory.
	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusCompleted {
		t.Errorf("session status: got %q, want COMPLETED", snap.Status)
	}
	if snap.Outcome == nil || *snap.Outcome != store.OutcomeWhite {
		t.Errorf("session outcome: got %v, want WHITE", snap.Outcome)
	}
	if snap.OutcomeReason == nil || *snap.OutcomeReason != store.OutcomeReasonCheckmate {
		t.Errorf("session outcomeReason: got %v, want CHECKMATE", snap.OutcomeReason)
	}

	// Verify DB state.
	game, err := store.NewGameStore(testPool).GetGame(ctx, moveTestGameID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != store.GameStatusCompleted {
		t.Errorf("DB game.Status: got %q, want COMPLETED", game.Status)
	}
	if game.Outcome == nil || *game.Outcome != store.OutcomeWhite {
		t.Errorf("DB game.Outcome: got %v, want WHITE", game.Outcome)
	}
	if game.OutcomeReason == nil || *game.OutcomeReason != store.OutcomeReasonCheckmate {
		t.Errorf("DB game.OutcomeReason: got %v, want CHECKMATE", game.OutcomeReason)
	}
}

func TestMoveProcessor_GameNotActive_RejectsMove(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, moveTestWhiteID)
	mustCreateUser(t, moveTestBlackID)

	ctx := context.Background()
	bus := NewLocalEventBus()

	// Create session but leave it in WAITING_FOR_PLAYER (do not call Transition).
	gs := store.NewGameStore(testPool)
	if err := gs.CreateGame(ctx, &store.Game{
		ID:            moveTestGameID,
		PlayerWhiteID: moveTestWhiteID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   600_000,
		BlackTimeMs:   600_000,
	}); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	session := NewGameSession(moveTestGameID, moveTestWhiteID)
	processor := newMoveProcessor(bus)

	err := processor.ProcessMove(ctx, session, store.ColorWhite, "e4")
	if err == nil {
		t.Fatal("expected error for non-active game, got nil")
	}

	var rejection *MoveRejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("expected *MoveRejectionError, got: %T %v", err, err)
	}
	if rejection.Reason != RejectReasonGameNotActive {
		t.Errorf("rejection.Reason: got %q, want %q", rejection.Reason, RejectReasonGameNotActive)
	}
}
