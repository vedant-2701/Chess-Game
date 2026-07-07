package game_test

import (
	"errors"
	"sync"
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

	if _, err := s.RegisterConnection(store.ColorWhite, conn); err != nil {
		t.Fatalf("expected RegisterConnection to succeed, got: %v", err)
	}
}

func TestRegisterConnection_OccupiedSlotReturnsError(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_, _ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-1"))

	_, err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-2"))
	if err == nil {
		t.Fatal("expected error when registering into an occupied slot")
	}
	if !errors.Is(err, game.ErrConnectionOccupied) {
		t.Errorf("expected ErrConnectionOccupied, got: %v", err)
	}
}

// TestRegisterConnection_ConcurrentBothConnect_ExactlyOneActivates reproduces
// ADR-017's race directly: two goroutines calling RegisterConnection for
// different colors on the same session, concurrently. Before the fix
// (RegisterConnection + BothPlayersConnected + Transition as three separate
// lock acquisitions inside Manager.HandleConnect), both goroutines could
// observe "both connected" as true and both attempt the WAITING→ACTIVE
// transition — the loser errored out of HandleConnect after already having
// registered its connection into the session.
//
// This test proves the fix at the GameSession level, where the atomic
// compound operation now lives. Run under -race. Across many trials, exactly
// one of the two concurrent registrations must report activated=true, and
// the session must end up ACTIVE regardless of interleaving.
//
// A sequential-only test (as every other test in this file is) cannot catch
// this class of bug — see CLAUDE.md Non-Negotiable Constraint #10 and
// ADR-016's precedent, where a sequential-only test passed while the
// underlying concurrent bug remained unfixed.
func TestRegisterConnection_ConcurrentBothConnect_ExactlyOneActivates(t *testing.T) {
	const trials = 200

	for i := 0; i < trials; i++ {
		s := game.NewGameSession("game-1", "user-white")

		var wg sync.WaitGroup
		activations := make(chan bool, 2)
		errsCh := make(chan error, 2)

		wg.Add(2)
		go func() {
			defer wg.Done()
			activated, err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-w"))
			activations <- activated
			errsCh <- err
		}()
		go func() {
			defer wg.Done()
			activated, err := s.RegisterConnection(store.ColorBlack, fakeConn("conn-b"))
			activations <- activated
			errsCh <- err
		}()
		wg.Wait()
		close(activations)
		close(errsCh)

		for err := range errsCh {
			if err != nil {
				t.Fatalf("trial %d: unexpected error from concurrent RegisterConnection: %v", i, err)
			}
		}

		activatedCount := 0
		for activated := range activations {
			if activated {
				activatedCount++
			}
		}
		if activatedCount != 1 {
			t.Fatalf("trial %d: expected exactly one concurrent caller to activate the game, got %d", i, activatedCount)
		}

		if snap := s.CurrentStateSnapshot(); snap.Status != store.GameStatusActive {
			t.Fatalf("trial %d: expected session to be ACTIVE after both connected, got %s", i, snap.Status)
		}
	}
}

func TestReplaceConnection_OverwritesExisting(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_, _ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-old"))

	// ReplaceConnection must not return an error and must succeed even when
	// the slot is occupied (this is the reconnection path).
	s.ReplaceConnection(store.ColorWhite, fakeConn("conn-new"))

	// After replace, registering again should still fail (slot is occupied
	// by the new connection).
	_, err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-3"))
	if !errors.Is(err, game.ErrConnectionOccupied) {
		t.Errorf("expected slot to remain occupied after ReplaceConnection, got: %v", err)
	}
}

func TestClearConnection_SetsSlotToNil(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")
	_, _ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-1"))

	s.ClearConnection(store.ColorWhite)

	// After clearing, registering again should succeed (slot is free).
	if _, err := s.RegisterConnection(store.ColorWhite, fakeConn("conn-2")); err != nil {
		t.Errorf("expected slot to be free after ClearConnection, got: %v", err)
	}
}

// --- BothPlayersConnected ---

func TestBothPlayersConnected(t *testing.T) {
	s := game.NewGameSession("game-1", "user-white")

	if s.BothPlayersConnected() {
		t.Fatal("expected false with no connections registered")
	}

	_, _ = s.RegisterConnection(store.ColorWhite, fakeConn("conn-w"))
	if s.BothPlayersConnected() {
		t.Fatal("expected false with only White connected")
	}

	_, _ = s.RegisterConnection(store.ColorBlack, fakeConn("conn-b"))
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