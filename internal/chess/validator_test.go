package chess

import (
	"errors"
	"testing"

	"github.com/notnil/chess"
)

// Scholar's mate: 1.e4 e5 2.Bc4 Nc6 3.Qh5 Nf6?? 4.Qxf7#
// After 4.Qxf7# it is Black's turn, Black is in check with no legal moves.
var scholarsMate = []string{"e4", "e5", "Bc4", "Nc6", "Qh5", "Nf6", "Qxf7"}

// Stalemate FEN: Black king on a8, White queen on b6, White king on c6.
// Black to move, not in check, no legal moves.
const stalemateFEN = "k7/8/1QK5/8/8/8/8/8 b - - 0 1"

// Pre-stalemate FEN: White to move; playing Qb7 produces stalemate.
const preStalemateFEN = "7k/8/4Q1K1/8/8/8/8/8 w - - 0 1"

// En passant FEN: White pawn on e5, Black just played d7-d5 (en passant target d6).
const enPassantFEN = "rnbqkbnr/ppp1pppp/8/3pP3/8/8/PPPP1PPP/RNBQKBNR w KQkq d6 0 2"

// Castling FEN: White can castle both sides, pieces cleared.
const castlingFEN = "r3k2r/pppppppp/8/8/8/8/PPPPPPPP/R3K2R w KQkq - 0 1"

// ---- helpers ----------------------------------------------------------------

func mustGameFromMoves(t *testing.T, moves []string) *chess.Game {
	t.Helper()
	g, err := GameFromMoves(moves)
	if err != nil {
		t.Fatalf("mustGameFromMoves: %v", err)
	}
	return g
}

func mustGameFromFEN(t *testing.T, fen string) *chess.Game {
	t.Helper()
	g, err := GameFromFEN(fen)
	if err != nil {
		t.Fatalf("mustGameFromFEN(%q): %v", fen, err)
	}
	return g
}

// ---- NewValidator -----------------------------------------------------------

func TestNewValidator(t *testing.T) {
	v := NewValidator()
	if v == nil {
		t.Fatal("NewValidator returned nil")
	}
}

// ---- NewGame ----------------------------------------------------------------

func TestNewGame_StartingPosition(t *testing.T) {
	g := NewGame()
	if g == nil {
		t.Fatal("NewGame returned nil")
	}
	const startFEN = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
	if got := CurrentFEN(g); got != startFEN {
		t.Errorf("NewGame FEN = %q, want %q", got, startFEN)
	}
}

// ---- GameFromFEN ------------------------------------------------------------

func TestGameFromFEN(t *testing.T) {
	tests := []struct {
		name    string
		fen     string
		wantErr bool
	}{
		{
			name:    "starting position",
			fen:     "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
			wantErr: false,
		},
		{
			name:    "en passant position",
			fen:     enPassantFEN,
			wantErr: false,
		},
		{
			name:    "stalemate position",
			fen:     stalemateFEN,
			wantErr: false,
		},
		{
			name:    "invalid FEN empty string",
			fen:     "",
			wantErr: true,
		},
		{
			name:    "invalid FEN garbage",
			fen:     "not a fen at all",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := GameFromFEN(tt.fen)
			if (err != nil) != tt.wantErr {
				t.Errorf("GameFromFEN(%q) error = %v, wantErr %v", tt.fen, err, tt.wantErr)
			}
			if !tt.wantErr && g == nil {
				t.Error("GameFromFEN returned nil game with no error")
			}
			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidFEN) {
				t.Errorf("expected ErrInvalidFEN, got %v", err)
			}
		})
	}
}

// ---- FEN round-trip ---------------------------------------------------------

func TestFENRoundTrip(t *testing.T) {
	fens := []string{
		"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
		enPassantFEN,
		stalemateFEN,
		castlingFEN,
	}
	for _, fen := range fens {
		t.Run(fen, func(t *testing.T) {
			g, err := GameFromFEN(fen)
			if err != nil {
				t.Fatalf("GameFromFEN: %v", err)
			}
			if got := CurrentFEN(g); got != fen {
				t.Errorf("FEN round-trip\n  input: %q\n  got:   %q", fen, got)
			}
		})
	}
}

// ---- GameFromMoves ----------------------------------------------------------

func TestGameFromMoves(t *testing.T) {
	t.Run("valid sequence", func(t *testing.T) {
		g, err := GameFromMoves([]string{"e4", "e5", "Nf3", "Nc6"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h := MoveHistory(g); len(h) != 4 {
			t.Errorf("MoveHistory len = %d, want 4", len(h))
		}
	})

	t.Run("empty list returns starting position", func(t *testing.T) {
		g, err := GameFromMoves([]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		const startFEN = "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"
		if got := CurrentFEN(g); got != startFEN {
			t.Errorf("FEN = %q, want %q", got, startFEN)
		}
	})

	t.Run("illegal move in sequence returns ErrIllegalMove", func(t *testing.T) {
		_, err := GameFromMoves([]string{"e4", "e4"}) // e4 again — Black to move, wrong square
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrIllegalMove) {
			t.Errorf("expected ErrIllegalMove, got %v", err)
		}
	})
}

// ---- ValidateMove -----------------------------------------------------------

func TestValidateMove(t *testing.T) {
	v := NewValidator()

	enPassantGame := mustGameFromFEN(t, enPassantFEN)
	castlingGame := mustGameFromFEN(t, castlingFEN)

	tests := []struct {
		name    string
		game    *chess.Game
		san     string
		wantErr bool
	}{
		{"valid e4", NewGame(), "e4", false},
		{"valid d4", NewGame(), "d4", false},
		{"wrong turn e5 (Black pawn, White to move)", NewGame(), "e5", true},
		{"nonsense SAN", NewGame(), "z9", true},
		{"empty SAN", NewGame(), "", true},
		{"en passant exd6", enPassantGame, "exd6", false},
		{"kingside castling O-O", castlingGame, "O-O", false},
		{"queenside castling O-O-O", castlingGame, "O-O-O", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fenBefore := CurrentFEN(tt.game)

			err := v.ValidateMove(tt.game, tt.san)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMove(%q) error = %v, wantErr %v", tt.san, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrIllegalMove) {
				t.Errorf("expected ErrIllegalMove, got %v", err)
			}
			// Critical invariant: ValidateMove must never mutate the game.
			if fenAfter := CurrentFEN(tt.game); fenBefore != fenAfter {
				t.Errorf("ValidateMove mutated game state!\n  before: %q\n  after:  %q", fenBefore, fenAfter)
			}
		})
	}
}

// ---- ApplyMove --------------------------------------------------------------

func TestApplyMove(t *testing.T) {
	v := NewValidator()

	t.Run("valid move advances state to correct FEN", func(t *testing.T) {
		g := NewGame()
		if err := v.ApplyMove(g, "e4"); err != nil {
			t.Fatalf("ApplyMove(e4): %v", err)
		}
		const wantFEN = "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1"
		if got := CurrentFEN(g); got != wantFEN {
			t.Errorf("FEN after e4\n  got:  %q\n  want: %q", got, wantFEN)
		}
		if h := MoveHistory(g); len(h) != 1 || h[0] != "e4" {
			t.Errorf("MoveHistory = %v, want [e4]", h)
		}
	})

	t.Run("illegal move returns error and does not advance state", func(t *testing.T) {
		g := NewGame()
		fenBefore := CurrentFEN(g)
		if err := v.ApplyMove(g, "e5"); err == nil {
			t.Fatal("expected error for illegal move, got nil")
		}
		if fenAfter := CurrentFEN(g); fenBefore != fenAfter {
			t.Error("ApplyMove with illegal move changed game state — board corrupted")
		}
	})
}

// ---- Validate-then-Apply pipeline correctness --------------------------------

// TestValidateThenApply verifies the ADR-013 pipeline contract: validate at T,
// persist at T+DB, apply at T+1 produces correct state. When no other goroutine
// touches the game between calls (enforced by session mutex in production), the
// result must be identical to a single MoveStr call.
func TestValidateThenApply(t *testing.T) {
	v := NewValidator()

	moves := []string{"e4", "e5", "Nf3", "Nc6", "Bc4", "Bc5"}
	reference := mustGameFromMoves(t, moves)
	referenceFEN := CurrentFEN(reference)

	pipeline := mustGameFromMoves(t, moves[:len(moves)-1])
	nextMove := moves[len(moves)-1]

	if err := v.ValidateMove(pipeline, nextMove); err != nil {
		t.Fatalf("ValidateMove(%q): %v", nextMove, err)
	}
	// DB write would happen here in production.
	if err := v.ApplyMove(pipeline, nextMove); err != nil {
		t.Fatalf("ApplyMove(%q): %v", nextMove, err)
	}

	if got := CurrentFEN(pipeline); got != referenceFEN {
		t.Errorf("validate-then-apply FEN mismatch\n  got:  %q\n  want: %q", got, referenceFEN)
	}
}

// ---- DetectOutcome ----------------------------------------------------------

func TestDetectOutcome_NoOutcome(t *testing.T) {
	v := NewValidator()
	outcome, hasOutcome := v.DetectOutcome(NewGame())
	if hasOutcome {
		t.Errorf("DetectOutcome on starting position: hasOutcome = true, outcome = %+v", outcome)
	}
	if outcome != nil {
		t.Errorf("DetectOutcome on starting position returned non-nil outcome: %+v", outcome)
	}
}

func TestDetectOutcome_NoOutcome_MidGame(t *testing.T) {
	v := NewValidator()
	g := mustGameFromMoves(t, []string{"e4", "e5", "Nf3", "Nc6"})
	outcome, hasOutcome := v.DetectOutcome(g)
	if hasOutcome {
		t.Errorf("DetectOutcome mid-game: hasOutcome = true, outcome = %+v", outcome)
	}
	if outcome != nil {
		t.Errorf("DetectOutcome mid-game returned non-nil outcome: %+v", outcome)
	}
}

func TestDetectOutcome_Checkmate_ScholarsMate(t *testing.T) {
	v := NewValidator()
	// 1.e4 e5 2.Bc4 Nc6 3.Qh5 Nf6?? 4.Qxf7# — White delivers checkmate.
	g := mustGameFromMoves(t, scholarsMate)

	outcome, hasOutcome := v.DetectOutcome(g)
	if !hasOutcome {
		t.Fatal("DetectOutcome after Scholar's mate: hasOutcome = false, want true")
	}
	if outcome.Winner != "WHITE" {
		t.Errorf("outcome.Winner = %q, want WHITE", outcome.Winner)
	}
	if outcome.Reason != "CHECKMATE" {
		t.Errorf("outcome.Reason = %q, want CHECKMATE", outcome.Reason)
	}
}

// TestDetectOutcome_Stalemate tests the realistic pipeline path: stalemate is
// detected after ApplyMove, not from FEN load. (notnil/chess only sets Game.Outcome()
// after a Move or MoveStr call, not on FEN construction — this is expected library behavior.)
func TestDetectOutcome_Stalemate(t *testing.T) {
	v := NewValidator()
	// White plays Qb7: Black king on a8 is not in check and has no legal moves.
	g := mustGameFromFEN(t, preStalemateFEN)

	if err := v.ApplyMove(g, "Qf7"); err != nil {
		t.Fatalf("ApplyMove(Qb7): %v", err)
	}

	outcome, hasOutcome := v.DetectOutcome(g)
	if !hasOutcome {
		t.Fatal("DetectOutcome after stalemate move: hasOutcome = false, want true")
	}
	if outcome.Winner != "DRAW" {
		t.Errorf("outcome.Winner = %q, want DRAW", outcome.Winner)
	}
	if outcome.Reason != "STALEMATE" {
		t.Errorf("outcome.Reason = %q, want STALEMATE", outcome.Reason)
	}
}

// ---- MoveHistory ------------------------------------------------------------

func TestMoveHistory(t *testing.T) {
	tests := []struct {
		name  string
		moves []string
	}{
		{"no moves", []string{}},
		{"one move", []string{"e4"}},
		{"four moves", []string{"e4", "e5", "Nf3", "Nc6"}},
		// The last move "Qxf7" is encoded as "Qxf7#" by AlgebraicNotation (check suffix added).
		// Input to GameFromMoves can omit the suffix; MoveHistory always returns the annotated form.
		{"scholars mate", []string{"e4", "e5", "Bc4", "Nc6", "Qh5", "Nf6", "Qxf7#"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := mustGameFromMoves(t, tt.moves)
			history := MoveHistory(g)

			// Must be non-nil even when empty (JSON serialization requirement).
			if history == nil {
				t.Error("MoveHistory returned nil, want non-nil empty slice")
			}
			if len(history) != len(tt.moves) {
				t.Errorf("MoveHistory len = %d, want %d", len(history), len(tt.moves))
			}
			for i, want := range tt.moves {
				if i >= len(history) {
					break
				}
				if history[i] != want {
					t.Errorf("MoveHistory[%d] = %q, want %q", i, history[i], want)
				}
			}
		})
	}
}

// ---- En passant and castling full-pipeline tests ----------------------------

func TestEnPassant(t *testing.T) {
	v := NewValidator()
	g := mustGameFromFEN(t, enPassantFEN)
	fenBefore := CurrentFEN(g)

	if err := v.ValidateMove(g, "exd6"); err != nil {
		t.Fatalf("ValidateMove(exd6): %v", err)
	}
	// ValidateMove must not mutate.
	if CurrentFEN(g) != fenBefore {
		t.Error("ValidateMove(exd6) mutated game state")
	}

	if err := v.ApplyMove(g, "exd6"); err != nil {
		t.Fatalf("ApplyMove(exd6): %v", err)
	}
	if CurrentFEN(g) == fenBefore {
		t.Error("ApplyMove(exd6) did not change FEN")
	}
	if h := MoveHistory(g); len(h) == 0 || h[len(h)-1] != "exd6" {
		t.Errorf("MoveHistory last = %v, want exd6", h)
	}
}

func TestCastling(t *testing.T) {
	v := NewValidator()

	for _, san := range []string{"O-O", "O-O-O"} {
		t.Run(san, func(t *testing.T) {
			g := mustGameFromFEN(t, castlingFEN)
			fenBefore := CurrentFEN(g)

			if err := v.ValidateMove(g, san); err != nil {
				t.Fatalf("ValidateMove(%q): %v", san, err)
			}
			if CurrentFEN(g) != fenBefore {
				t.Errorf("ValidateMove(%q) mutated game state", san)
			}

			if err := v.ApplyMove(g, san); err != nil {
				t.Fatalf("ApplyMove(%q): %v", san, err)
			}
			if h := MoveHistory(g); len(h) == 0 || h[len(h)-1] != san {
				t.Errorf("MoveHistory last = %v, want %q", h, san)
			}
		})
	}
}