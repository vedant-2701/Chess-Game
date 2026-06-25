//go:build integration

package store

import (
	"context"
	"testing"
)

func TestMoveStore_SaveMove(t *testing.T) {
	ms := newMoveStore()
	ctx := context.Background()

	t.Run("inserts move and populates ID and PlayedAt", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		move := &Move{
			GameID:     testGameID,
			MoveNumber: 1,
			Color:      ColorWhite,
			SAN:        "e4",
			FENAfter:   "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
		}

		if err := ms.SaveMove(ctx, move); err != nil {
			t.Fatalf("SaveMove: %v", err)
		}
		if move.ID == 0 {
			t.Error("ID should be populated by RETURNING clause, got 0")
		}
		if move.PlayedAt.IsZero() {
			t.Error("PlayedAt should be populated by RETURNING clause, got zero")
		}
	})

	t.Run("duplicate move_number for same game is rejected", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		first := &Move{
			GameID:     testGameID,
			MoveNumber: 1,
			Color:      ColorWhite,
			SAN:        "e4",
			FENAfter:   "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
		}
		if err := ms.SaveMove(ctx, first); err != nil {
			t.Fatalf("first SaveMove: %v", err)
		}

		duplicate := &Move{
			GameID:     testGameID,
			MoveNumber: 1, // same move number — violates UNIQUE index
			Color:      ColorWhite,
			SAN:        "d4",
			FENAfter:   "rnbqkbnr/pppppppp/8/8/3P4/8/PPP1PPPP/RNBQKBNR b KQkq d3 0 1",
		}
		err := ms.SaveMove(ctx, duplicate)
		if err == nil {
			t.Error("expected error for duplicate move_number, got nil")
		}
	})
}

func TestMoveStore_GetMovesForGame(t *testing.T) {
	ms := newMoveStore()
	ctx := context.Background()

	t.Run("returns empty non-nil slice for game with no moves", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		moves, err := ms.GetMovesForGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetMovesForGame: %v", err)
		}
		if moves == nil {
			t.Error("expected non-nil empty slice, got nil")
		}
		if len(moves) != 0 {
			t.Errorf("expected 0 moves, got %d", len(moves))
		}
	})

	t.Run("returns moves in ascending move_number order", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)
		mustCreateGame(t, testGameID, testWhiteID)

		// Insert moves representing: 1. e4 e5 2. Nf3
		movesToInsert := []struct {
			num   int
			color Color
			san   string
			fen   string
		}{
			{1, ColorWhite, "e4", "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"},
			{2, ColorBlack, "e5", "rnbqkbnr/pppp1ppp/8/4p3/4P3/8/PPPP1PPP/RNBQKBNR w KQkq e6 0 2"},
			{3, ColorWhite, "Nf3", "rnbqkbnr/pppp1ppp/8/4p3/4P3/5N2/PPPP1PPP/RNBQKB1R b KQkq - 1 2"},
		}

		for _, m := range movesToInsert {
			move := &Move{
				GameID:     testGameID,
				MoveNumber: m.num,
				Color:      m.color,
				SAN:        m.san,
				FENAfter:   m.fen,
			}
			if err := ms.SaveMove(ctx, move); err != nil {
				t.Fatalf("SaveMove moveNumber=%d: %v", m.num, err)
			}
		}

		got, err := ms.GetMovesForGame(ctx, testGameID)
		if err != nil {
			t.Fatalf("GetMovesForGame: %v", err)
		}
		if len(got) != len(movesToInsert) {
			t.Fatalf("expected %d moves, got %d", len(movesToInsert), len(got))
		}

		for i, want := range movesToInsert {
			m := got[i]
			if m.MoveNumber != want.num {
				t.Errorf("[%d] MoveNumber: got %d, want %d", i, m.MoveNumber, want.num)
			}
			if m.Color != want.color {
				t.Errorf("[%d] Color: got %q, want %q", i, m.Color, want.color)
			}
			if m.SAN != want.san {
				t.Errorf("[%d] SAN: got %q, want %q", i, m.SAN, want.san)
			}
			if m.GameID != testGameID {
				t.Errorf("[%d] GameID: got %q, want %q", i, m.GameID, testGameID)
			}
			if m.ID == 0 {
				t.Errorf("[%d] ID should be non-zero", i)
			}
			if m.PlayedAt.IsZero() {
				t.Errorf("[%d] PlayedAt should be non-zero", i)
			}
		}
	})

	t.Run("moves from different games are isolated", func(t *testing.T) {
		truncateAll(t)
		mustCreateUser(t, testWhiteID)

		gameA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		gameB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		mustCreateGame(t, gameA, testWhiteID)
		mustCreateGame(t, gameB, testWhiteID)

		if err := ms.SaveMove(ctx, &Move{
			GameID:     gameA,
			MoveNumber: 1,
			Color:      ColorWhite,
			SAN:        "e4",
			FENAfter:   StartingFEN,
		}); err != nil {
			t.Fatalf("SaveMove gameA: %v", err)
		}

		movesB, err := ms.GetMovesForGame(ctx, gameB)
		if err != nil {
			t.Fatalf("GetMovesForGame gameB: %v", err)
		}
		if len(movesB) != 0 {
			t.Errorf("gameB should have 0 moves, got %d", len(movesB))
		}
	})
}
