package game_test

import (
	"errors"
	"testing"

	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// fakeConn returns a *ws.Connection with a nil underlying websocket. Safe for
// tests that only check whether a slot is nil/non-nil and never trigger Send,
// Close, WriteLoop, or ReadLoop.
func fakeConn(id string) *ws.Connection {
	return ws.NewConnection(id, nil)
}

// --- State machine ---

func TestTransition_ValidEdges(t *testing.T) {
	tests := []struct {
		name    string
		from    store.GameStatus
		to      store.GameStatus
	}{
		{"waiting to active", store.GameStatusWaiting, store.GameStatusActive},
		{"active to completed", store.GameStatusActive, store.GameStatusCompleted},
		{"active to abandoned", store.GameStatusActive, store.GameStatusAbandoned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := game.NewGameSession("game-1", "user-white")
			// Drive the session to the desired starting state.
			if tt.from == store.GameStatusActive {
				if err := s.Transition(store.GameStatusActive); err != nil {
					t.Fatalf("setup transition to ACTIVE failed: %v", err)
				}
			}
			if err := s.Transition(tt.to); err != nil {
				t.Errorf("Transition(%s→%s) returned unexpected error: %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestTransition_InvalidEdges(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*game.GameSession) // drive to starting state
		attempt store.GameStatus
	}{
		{
			"waiting to completed",
			func(s *game.GameSession) {},
			store.GameStatusCompleted,
		},
		{
			"waiting to abandoned",
			func(s *game.GameSession) {},
			store.GameStatusAbandoned,
		},
		{
			"active to waiting",
			func(s *game.GameSession) { _ = s.Transition(store.GameStatusActive) },
			store.GameStatusWaiting,
		},
		{
			"completed to active",
			func(s *game.GameSession) {
				_ = s.Transition(store.GameStatusActive)
				_ = s.Transition(store.GameStatusCompleted)
			},
			store.GameStatusActive,
		},
		{
			"completed to abandoned",
			func(s *game.GameSession) {
				_ = s.Transition(store.GameStatusActive)
				_ = s.Transition(store.GameStatusCompleted)
			},
			store.GameStatusAbandoned,
		},
		{
			"abandoned to active",
			func(s *game.GameSession) {
				_ = s.Transition(store.GameStatusActive)
				_ = s.Transition(store.GameStatusAbandoned)
			},
			store.GameStatusActive,
		},
		{
			"abandoned to completed",
			func(s *game.GameSession) {
				_ = s.Transition(store.GameStatusActive)
				_ = s.Transition(store.GameStatusAbandoned)
			},
			store.GameStatusCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := game.NewGameSession("game-1", "user-white")
			tt.setup(s)
			err := s.Transition(tt.attempt)
			if err == nil {
				t.Fatalf("Transition to %s should have returned an error but did not", tt.attempt)
			}
			if !errors.Is(err, game.ErrInvalidTransition) {
				t.Errorf("expected ErrInvalidTransition, got: %v", err)
			}
		})
	}
}

// --- Connection management ---

func TestRegisterConnection_FirstConnectionSucceeds(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	conn := fakeConn("conn-1")

	if err := s.RegisterConnection(store.ColorWhite, conn); err != nil {
		t.Fatalf("expected RegisterConnection to succeed, got: %v", err)
	}
}

func TestRegisterConnection_OccupiedSlotReturnsError(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-1"))

	err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-2"))
	if err == nil {
		t.Fatal("expected error when registering into an occupied slot")
	}
	if !errors.Is(err, game.ErrConnectionOccupied) {
		t.Errorf("expected ErrConnectionOccupied, got: %v", err)
	}
}

func TestReplaceConnection_OverwritesExisting(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-old"))

	// ReplaceConnection must not return an error and must succeed even when
	// the slot is occupied (this is the reconnection path).
	s.ReplaceConnection(store.ColorWhite, fakeConn("conn-new"))

	// After replace, registering again should still fail (slot is occupied
	// by the new connection).
	err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-3"))
	if !errors.Is(err, game.ErrConnectionOccupied) {
		t.Errorf("expected slot to remain occupied after ReplaceConnection, got: %v", err)
	}
}

func TestClearConnection_SetsSlotToNil(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-1"))

	s.ClearConnection(store.ColorWhite)

	// After clearing, registering again should succeed (slot is free).
	if err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-2")); err != nil {
		t.Errorf("expected slot to be free after ClearConnection, got: %v", err)
	}
}

// --- BothPlayersConnected ---

func TestBothPlayersConnected(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")

	if s.BothPlayersConnected() {
		t.Fatal("expected false with no connections registered")
	}

	_ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-w"))
	if s.BothPlayersConnected() {
		t.Fatal("expected false with only White connected")
	}

	_ = s.RegisterConnection(store.ColorBlack, fakeConn("conn-b"))
	if !s.BothPlayersConnected() {
		t.Fatal("expected true with both players connected")
	}

	s.ClearConnection(store.ColorBlack)
	if s.BothPlayersConnected() {
		t.Fatal("expected false after Black disconnected")
	}
}

// --- CurrentStateSnapshot ---

func TestCurrentStateSnapshot_InitialState(t *testing.T) {
	s := game.NewGameSession("game-42", "user-white-id")
	snap := s.CurrentStateSnapshot()

	if snap.ID != "game-42" {
		t.Errorf("ID: got %q, want %q", snap.ID, "game-42")
	}
	if snap.Status != store.GameStatusWaiting {
		t.Errorf("Status: got %q, want %q", snap.Status, store.GameStatusWaiting)
	}
	if snap.PlayerWhiteID != "user-white-id" {
		t.Errorf("PlayerWhiteID: got %q, want %q", snap.PlayerWhiteID, "user-white-id")
	}
	if snap.PlayerBlackID != "" {
		t.Errorf("PlayerBlackID: expected empty string before SetPlayerBlack, got %q", snap.PlayerBlackID)
	}
	if snap.Turn != store.ColorWhite {
		t.Errorf("Turn: got %q, want WHITE (starting position)", snap.Turn)
	}
	if len(snap.Moves) != 0 {
		t.Errorf("Moves: expected empty at start, got %v", snap.Moves)
	}
	if snap.WhiteTimeMs != game.InitialTimeMs {
		t.Errorf("WhiteTimeMs: got %d, want %d", snap.WhiteTimeMs, game.InitialTimeMs)
	}
	if snap.BlackTimeMs != game.InitialTimeMs {
		t.Errorf("BlackTimeMs: got %d, want %d", snap.BlackTimeMs, game.InitialTimeMs)
	}
	if snap.Outcome != nil {
		t.Errorf("Outcome: expected nil at start, got %v", snap.Outcome)
	}
}

func TestCurrentStateSnapshot_ReflectsSetPlayerBlack(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	s.SetPlayerBlack("user-black-id")

	snap := s.CurrentStateSnapshot()
	if snap.PlayerBlackID != "user-black-id" {
		t.Errorf("PlayerBlackID: got %q, want %q", snap.PlayerBlackID, "user-black-id")
	}
}