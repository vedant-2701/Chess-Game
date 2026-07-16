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
		fromStatus    GameStatus
		newStatus     GameStatus
		outcome       *GameOutcome
		wantStatus    GameStatus
		wantOutcome   *Outcome
		wantOutReason *OutcomeReason
	}{
		{
			name:          "WAITING to ACTIVE (no outcome)",
			fromStatus:    GameStatusWaiting,
			newStatus:     GameStatusActive,
			outcome:       nil,
			wantStatus:    GameStatusActive,
			wantOutcome:   nil,
			wantOutReason: nil,
		},
		{
			name:          "ACTIVE to COMPLETED with checkmate",
			fromStatus:    GameStatusActive,
			newStatus:     GameStatusCompleted,
			outcome:       &GameOutcome{Outcome: OutcomeWhite, Reason: OutcomeReasonCheckmate},
			wantStatus:    GameStatusCompleted,
			wantOutcome:   func() *Outcome { o := OutcomeWhite; return &o }(),
			wantOutReason: func() *OutcomeReason { r := OutcomeReasonCheckmate; return &r }(),
		},
		{
			name:          "ACTIVE to COMPLETED with timeout",
			fromStatus:    GameStatusActive,
			newStatus:     GameStatusCompleted,
			outcome:       &GameOutcome{Outcome: OutcomeBlack, Reason: OutcomeReasonTimeout},
			wantStatus:    GameStatusCompleted,
			wantOutcome:   func() *Outcome { o := OutcomeBlack; return &o }(),
			wantOutReason: func() *OutcomeReason { r := OutcomeReasonTimeout; return &r }(),
		},
		{
			name:          "ACTIVE to ABANDONED",
			fromStatus:    GameStatusActive,
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

			// mustCreateGame leaves the row in WAITING_FOR_PLAYER. Advance it to
			// ACTIVE first for any case whose fromStatus is ACTIVE, so the
			// conditional UPDATE under test has a true starting state to match
			// against — otherwise every ACTIVE-fromStatus case would spuriously
			// hit the new ErrGameStatusConflict path instead of exercising a
			// genuine transition.
			if tt.fromStatus == GameStatusActive {
				if err := gs.UpdateGameStatus(ctx, testGameID, GameStatusWaiting, GameStatusActive, nil); err != nil {
					t.Fatalf("setup: advance to ACTIVE: %v", err)
				}
			}

			err := gs.UpdateGameStatus(ctx, testGameID, tt.fromStatus, tt.newStatus, tt.outcome)
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

	t.Run("returns ErrGameStatusConflict for unknown ID", func(t *testing.T) {
		// Per the new method's documented contract, a nonexistent row and a
		// row whose status doesn't match fromStatus are indistinguishable from
		// RowsAffected() alone, and every real call site already guarantees
		// existence before calling — so a missing row also surfaces as
		// ErrGameStatusConflict, not ErrGameNotFound. This is a deliberate
		// contract choice (see the method's doc comment), not an oversight.
		truncateAll(t)
		err := gs.UpdateGameStatus(ctx, "cccccccc-cccc-cccc-cccc-cccccccccccc", GameStatusWaiting, GameStatusActive, nil)
		if !errors.Is(err, ErrGameStatusConflict) {
			t.Errorf("expected ErrGameStatusConflict, got: %v", err)
		}
	})

	t.Run("returns ErrGameStatusConflict when fromStatus does not match current row status", func(t *testing.T) {
		// The actual regression case this fix exists for: a caller whose
		// belief about the game's prior status (fromStatus) is stale — e.g. a
		// second writer racing a terminal-state transition — must lose the
		// race cleanly via the predicate, not silently overwrite the winner.
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)
		// Row is WAITING_FOR_PLAYER. Ask for an ACTIVE→COMPLETED transition
		// (fromStatus=ACTIVE) against a row that is actually still WAITING.
		err := gs.UpdateGameStatus(ctx, testGameID, GameStatusActive, GameStatusCompleted,
			&GameOutcome{Outcome: OutcomeWhite, Reason: OutcomeReasonCheckmate})
		if !errors.Is(err, ErrGameStatusConflict) {
			t.Errorf("expected ErrGameStatusConflict, got: %v", err)
		}

		// Confirm the row was left untouched by the failed conditional write.
		game, getErr := gs.GetGame(ctx, testGameID)
		if getErr != nil {
			t.Fatalf("GetGame: %v", getErr)
		}
		if game.Status != GameStatusWaiting {
			t.Errorf("Status: got %q, want %q (write must not have applied)", game.Status, GameStatusWaiting)
		}
		if game.Outcome != nil {
			t.Errorf("Outcome: expected nil (write must not have applied), got %v", *game.Outcome)
		}
	})

	t.Run("concurrent racing writers: exactly one transition wins", func(t *testing.T) {
		// Reproduces the class of bug this predicate closes: two goroutines
		// both believing the game is ACTIVE and both racing a terminal
		// transition (e.g. resign vs. timeout in Phase 1; two instances
		// racing during TD-P2-001's window in Phase 2). Without the fromStatus
		// predicate, whichever write commits last would silently overwrite
		// the first writer's outcome. With it, exactly one of the two
		// conditional UPDATEs should affect a row.
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)
		if err := gs.UpdateGameStatus(ctx, testGameID, GameStatusWaiting, GameStatusActive, nil); err != nil {
			t.Fatalf("setup: advance to ACTIVE: %v", err)
		}

		type result struct {
			err error
		}
		results := make(chan result, 2)

		go func() {
			err := gs.UpdateGameStatus(ctx, testGameID, GameStatusActive, GameStatusCompleted,
				&GameOutcome{Outcome: OutcomeWhite, Reason: OutcomeReasonResignation})
			results <- result{err: err}
		}()
		go func() {
			err := gs.UpdateGameStatus(ctx, testGameID, GameStatusActive, GameStatusCompleted,
				&GameOutcome{Outcome: OutcomeBlack, Reason: OutcomeReasonTimeout})
			results <- result{err: err}
		}()

		r1, r2 := <-results, <-results

		successes, conflicts := 0, 0
		for _, r := range []result{r1, r2} {
			switch {
			case r.err == nil:
				successes++
			case errors.Is(r.err, ErrGameStatusConflict):
				conflicts++
			default:
				t.Fatalf("unexpected error: %v", r.err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("expected exactly 1 success and 1 conflict, got %d successes, %d conflicts", successes, conflicts)
		}

		// The persisted outcome must belong to whichever writer actually won —
		// not be some torn/mixed combination of both.
		game, err := gs.GetGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetGame: %v", err)
		}
		if game.Status != GameStatusCompleted {
			t.Errorf("Status: got %q, want %q", game.Status, GameStatusCompleted)
		}
		validPair := (*game.Outcome == OutcomeWhite && *game.OutcomeReason == OutcomeReasonResignation) ||
			(*game.Outcome == OutcomeBlack && *game.OutcomeReason == OutcomeReasonTimeout)
		if !validPair {
			t.Errorf("outcome/reason pair is not from either racing writer: outcome=%v reason=%v", *game.Outcome, *game.OutcomeReason)
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
		if err := gs.UpdateGameStatus(ctx, activeID, GameStatusWaiting, GameStatusActive, nil); err != nil {
			t.Fatalf("set active: %v", err)
		}
		if err := gs.UpdateGameStatus(ctx, doneID, GameStatusWaiting, GameStatusCompleted,
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
