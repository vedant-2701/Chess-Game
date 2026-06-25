//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
)

const (
	testWhiteID = "00000000-0000-0000-0000-000000000001"
	testBlackID = "00000000-0000-0000-0000-000000000002"
	testGameID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
)

func TestGameStore_CreateGame(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("inserts game with correct defaults", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)

		err := gs.CreateGame(ctx, &Game{
			ID:            testGameID,
			PlayerWhiteID: testWhiteID,
			CurrentFEN:    StartingFEN,
			WhiteTimeMs:   600_000,
			BlackTimeMs:   600_000,
		})
		if err != nil {
			t.Fatalf("CreateGame: %v", err)
		}

		// Verify by reading back
		game, err := gs.GetGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetGame after create: %v", err)
		}
		if game.ID != testGameID {
			t.Errorf("ID: got %q, want %q", game.ID, testGameID)
		}
		if game.Status != GameStatusWaiting {
			t.Errorf("Status: got %q, want %q", game.Status, GameStatusWaiting)
		}
		if game.PlayerWhiteID != testWhiteID {
			t.Errorf("PlayerWhiteID: got %q, want %q", game.PlayerWhiteID, testWhiteID)
		}
		if game.PlayerBlackID != nil {
			t.Errorf("PlayerBlackID: expected nil, got %q", *game.PlayerBlackID)
		}
		if game.CurrentFEN != StartingFEN {
			t.Errorf("CurrentFEN: got %q, want %q", game.CurrentFEN, StartingFEN)
		}
		if game.WhiteTimeMs != 600_000 {
			t.Errorf("WhiteTimeMs: got %d, want 600000", game.WhiteTimeMs)
		}
		if game.BlackTimeMs != 600_000 {
			t.Errorf("BlackTimeMs: got %d, want 600000", game.BlackTimeMs)
		}
		if game.Outcome != nil {
			t.Errorf("Outcome: expected nil, got %v", *game.Outcome)
		}
		if game.OutcomeReason != nil {
			t.Errorf("OutcomeReason: expected nil, got %v", *game.OutcomeReason)
		}
	})
}

func TestGameStore_GetGame(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("returns ErrGameNotFound for unknown ID", func(t *testing.T) {
		truncateAll(t)

		_, err := gs.GetGame(ctx, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		if !errors.Is(err, ErrGameNotFound) {
			t.Errorf("expected ErrGameNotFound, got: %v", err)
		}
	})
}

func TestGameStore_UpdateGameStatus(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	tests := []struct {
		name          string
		newStatus     GameStatus
		outcome       *GameOutcome
		wantStatus    GameStatus
		wantOutcome   *Outcome
		wantOutReason *OutcomeReason
	}{
		{
			name:          "WAITING to ACTIVE (no outcome)",
			newStatus:     GameStatusActive,
			outcome:       nil,
			wantStatus:    GameStatusActive,
			wantOutcome:   nil,
			wantOutReason: nil,
		},
		{
			name:          "ACTIVE to COMPLETED with checkmate",
			newStatus:     GameStatusCompleted,
			outcome:       &GameOutcome{Outcome: OutcomeWhite, Reason: OutcomeReasonCheckmate},
			wantStatus:    GameStatusCompleted,
			wantOutcome:   func() *Outcome { o := OutcomeWhite; return &o }(),
			wantOutReason: func() *OutcomeReason { r := OutcomeReasonCheckmate; return &r }(),
		},
		{
			name:          "ACTIVE to COMPLETED with timeout",
			newStatus:     GameStatusCompleted,
			outcome:       &GameOutcome{Outcome: OutcomeBlack, Reason: OutcomeReasonTimeout},
			wantStatus:    GameStatusCompleted,
			wantOutcome:   func() *Outcome { o := OutcomeBlack; return &o }(),
			wantOutReason: func() *OutcomeReason { r := OutcomeReasonTimeout; return &r }(),
		},
		{
			name:          "ACTIVE to ABANDONED",
			newStatus:     GameStatusAbandoned,
			outcome:       &GameOutcome{Outcome: OutcomeDraw, Reason: OutcomeReasonAbandoned},
			wantStatus:    GameStatusAbandoned,
			wantOutcome:   func() *Outcome { o := OutcomeDraw; return &o }(),
			wantOutReason: func() *OutcomeReason { r := OutcomeReasonAbandoned; return &r }(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			truncateAll(t)
			mustCreateUser(t, testWhiteID)
			mustCreateGame(t, testGameID, testWhiteID)

			err := gs.UpdateGameStatus(ctx, testGameID, tt.newStatus, tt.outcome)
			if err != nil {
				t.Fatalf("UpdateGameStatus: %v", err)
			}

			game, err := gs.GetGame(ctx, testGameID)
			if err != nil {
				t.Fatalf("GetGame: %v", err)
			}

			if game.Status != tt.wantStatus {
				t.Errorf("Status: got %q, want %q", game.Status, tt.wantStatus)
			}

			if tt.wantOutcome == nil {
				if game.Outcome != nil {
					t.Errorf("Outcome: expected nil, got %v", *game.Outcome)
				}
			} else {
				if game.Outcome == nil {
					t.Error("Outcome: expected non-nil, got nil")
				} else if *game.Outcome != *tt.wantOutcome {
					t.Errorf("Outcome: got %q, want %q", *game.Outcome, *tt.wantOutcome)
				}
			}

			if tt.wantOutReason == nil {
				if game.OutcomeReason != nil {
					t.Errorf("OutcomeReason: expected nil, got %v", *game.OutcomeReason)
				}
			} else {
				if game.OutcomeReason == nil {
					t.Error("OutcomeReason: expected non-nil, got nil")
				} else if *game.OutcomeReason != *tt.wantOutReason {
					t.Errorf("OutcomeReason: got %q, want %q", *game.OutcomeReason, *tt.wantOutReason)
				}
			}
		})
	}

	t.Run("returns ErrGameNotFound for unknown ID", func(t *testing.T) {
		truncateAll(t)
		err := gs.UpdateGameStatus(ctx, "cccccccc-cccc-cccc-cccc-cccccccccccc", GameStatusActive, nil)
		if !errors.Is(err, ErrGameNotFound) {
			t.Errorf("expected ErrGameNotFound, got: %v", err)
		}
	})
}

func TestGameStore_UpdateCurrentFEN(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("updates FEN correctly", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		const newFEN = "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"
		if err := gs.UpdateCurrentFEN(ctx, testGameID, newFEN); err != nil {
			t.Fatalf("UpdateCurrentFEN: %v", err)
		}

		game, err := gs.GetGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetGame: %v", err)
		}
		if game.CurrentFEN != newFEN {
			t.Errorf("CurrentFEN: got %q, want %q", game.CurrentFEN, newFEN)
		}
	})

	t.Run("returns ErrGameNotFound for unknown ID", func(t *testing.T) {
		truncateAll(t)
		err := gs.UpdateCurrentFEN(ctx, "dddddddd-dddd-dddd-dddd-dddddddddddd", StartingFEN)
		if !errors.Is(err, ErrGameNotFound) {
			t.Errorf("expected ErrGameNotFound, got: %v", err)
		}
	})
}

func TestGameStore_UpdatePlayerBlack(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("sets player_black_id", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateUser(t, testBlackID)
		mustCreateGame(t, testGameID, testWhiteID)

		if err := gs.UpdatePlayerBlack(ctx, testGameID, testBlackID); err != nil {
			t.Fatalf("UpdatePlayerBlack: %v", err)
		}

		game, err := gs.GetGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetGame: %v", err)
		}
		if game.PlayerBlackID == nil {
			t.Fatal("PlayerBlackID: expected non-nil, got nil")
		}
		if *game.PlayerBlackID != testBlackID {
			t.Errorf("PlayerBlackID: got %q, want %q", *game.PlayerBlackID, testBlackID)
		}
	})
}

func TestGameStore_GetActiveGames(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("returns only WAITING and ACTIVE games", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)

		waitingID := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		activeID := "ffffffff-ffff-ffff-ffff-ffffffffffff"
		doneID := "11111111-1111-1111-1111-111111111111"

		mustCreateGame(t, waitingID, testWhiteID)
		mustCreateGame(t, activeID, testWhiteID)
		mustCreateGame(t, doneID, testWhiteID)

		// Advance active and done games to their respective statuses
		if err := gs.UpdateGameStatus(ctx, activeID, GameStatusActive, nil); err != nil {
			t.Fatalf("set active: %v", err)
		}
		if err := gs.UpdateGameStatus(ctx, doneID, GameStatusCompleted,
			&GameOutcome{Outcome: OutcomeWhite, Reason: OutcomeReasonCheckmate}); err != nil {
			t.Fatalf("set completed: %v", err)
		}

		games, err := gs.GetActiveGames(ctx)
		if err != nil {
			t.Fatalf("GetActiveGames: %v", err)
		}
		if len(games) != 2 {
			t.Fatalf("expected 2 active games, got %d", len(games))
		}

		ids := map[string]bool{}
		for _, g := range games {
			ids[g.ID] = true
		}
		if !ids[waitingID] {
			t.Errorf("expected waitingID %q in results", waitingID)
		}
		if !ids[activeID] {
			t.Errorf("expected activeID %q in results", activeID)
		}
		if ids[doneID] {
			t.Errorf("completed game %q must not appear in active games", doneID)
		}
	})

	t.Run("returns empty slice when no active games", func(t *testing.T) {
		truncateAll(t)

		games, err := gs.GetActiveGames(ctx)
		if err != nil {
			t.Fatalf("GetActiveGames: %v", err)
		}
		if games == nil {
			t.Error("expected non-nil empty slice, got nil")
		}
		if len(games) != 0 {
			t.Errorf("expected 0 games, got %d", len(games))
		}
	})
}

func TestGameStore_UpdateClocks(t *testing.T) {
	gs := newGameStore()
	ctx := context.Background()

	t.Run("persists both clock values", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		const wantWhite, wantBlack = 597_843, 600_000
		if err := gs.UpdateClocks(ctx, testGameID, wantWhite, wantBlack); err != nil {
			t.Fatalf("UpdateClocks: %v", err)
		}

		game, err := gs.GetGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetGame: %v", err)
		}
		if game.WhiteTimeMs != wantWhite {
			t.Errorf("WhiteTimeMs: got %d, want %d", game.WhiteTimeMs, wantWhite)
		}
		if game.BlackTimeMs != wantBlack {
			t.Errorf("BlackTimeMs: got %d, want %d", game.BlackTimeMs, wantBlack)
		}
	})

	t.Run("returns ErrGameNotFound for unknown ID", func(t *testing.T) {
		truncateAll(t)
		err := gs.UpdateClocks(ctx, "22222222-2222-2222-2222-222222222222", 1000, 2000)
		if !errors.Is(err, ErrGameNotFound) {
			t.Errorf("expected ErrGameNotFound, got: %v", err)
		}
	})
}
