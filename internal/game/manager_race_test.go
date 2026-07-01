//go:build integration

package game

import (
	"context"
	"errors"
	"sync"
	"testing"

	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

// TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins is a regression test for
// the TOCTOU race in JoinGame described in ADR-016: two different users calling
// JoinGame for the same gameID concurrently, both racing GetGame's read against
// UpdatePlayerBlack's write. Before the fix, UpdatePlayerBlack's UPDATE had no
// WHERE guard on player_black_id, so both goroutines could pass the pre-flight
// GetGame check and the later write would silently win, letting the second
// caller believe they joined when they actually overwrote the first joiner.
//
// This test only proves anything meaningful if it (a) actually exercises two
// goroutines racing the same gameID, not sequential calls, and (b) asserts on
// the persisted DB row, not just in-process return values — a bug in the SQL
// WHERE clause would not necessarily be visible from Go-level return values
// alone if both writes still returned success against different rows or if
// RowsAffected checking were skipped.
func TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins(t *testing.T) {
	truncateAll(t)

	const whiteID = "30000000-0000-0000-0000-000000000001"
	const blackCandidateA = "30000000-0000-0000-0000-000000000002"
	const blackCandidateB = "30000000-0000-0000-0000-000000000003"
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackCandidateA)
	mustCreateUser(t, blackCandidateB)

	ctx := context.Background()

	// Single shared Manager: JoinGame has no per-caller mutable state of its
	// own (all mutable state lives in the DB row and in GameSession, both of
	// which are the actual subjects under test), so sharing one Manager across
	// both goroutines is the correct way to reproduce the real production
	// scenario of two concurrent WebSocket/HTTP requests hitting one process.
	registry := NewGameRegistry()
	bus := NewLocalEventBus()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	processor := NewMoveProcessor(validator, gameStore, moveStore, bus)
	m := NewManager(registry, processor, gameStore, moveStore, bus, "race-test-secret", validator)

	// Run many trials, each in a fresh game. A single trial can pass by luck
	// if the goroutine scheduler happens to fully serialize the two calls —
	// the whole point of the WHERE-clause fix is correctness under actual
	// interleaving, not just under lucky scheduling. Repetition increases
	// confidence the fix holds under real contention rather than one
	// favorable run.
	const trials = 20
	for trial := 0; trial < trials; trial++ {
		session, _, err := m.CreateGame(ctx, whiteID)
		if err != nil {
			t.Fatalf("trial %d: CreateGame: %v", trial, err)
		}
		gameID := session.ID

		var wg sync.WaitGroup
		results := make(chan struct {
			userID string
			token  string
			err    error
		}, 2)

		wg.Add(2)
		for _, candidate := range []string{blackCandidateA, blackCandidateB} {
			candidate := candidate
			go func() {
				defer wg.Done()
				token, err := m.JoinGame(ctx, gameID, candidate)
				results <- struct {
					userID string
					token  string
					err    error
				}{candidate, token, err}
			}()
		}
		wg.Wait()
		close(results)

		var successes, failures int
		var winnerID string
		for r := range results {
			switch {
			case r.err == nil:
				successes++
				winnerID = r.userID
				if r.token == "" {
					t.Errorf("trial %d: JoinGame succeeded for %s but returned empty token", trial, r.userID)
				}
			case errors.Is(r.err, ErrGameNotJoinable):
				failures++
			default:
				t.Errorf("trial %d: JoinGame for %s returned unexpected error: %v", trial, r.userID, r.err)
			}
		}

		if successes != 1 {
			t.Fatalf("trial %d: got %d successful JoinGame calls, want exactly 1 (successes=%d failures=%d)",
				trial, successes, successes, failures)
		}
		if failures != 1 {
			t.Fatalf("trial %d: got %d failed JoinGame calls, want exactly 1 rejected with ErrGameNotJoinable",
				trial, failures)
		}

		// Assert against the persisted DB row directly — the actual source of
		// truth the WHERE-clause fix protects. A bug that let both writes
		// "succeed" at the Go level but still only persist one winner would
		// be caught here even if the in-process return values above looked
		// fine by coincidence.
		game, err := gameStore.GetGame(ctx, gameID)
		if err != nil {
			t.Fatalf("trial %d: GetGame: %v", trial, err)
		}
		if game.PlayerBlackID == nil {
			t.Fatalf("trial %d: DB player_black_id is nil after a claimed successful join", trial)
		}
		if *game.PlayerBlackID != winnerID {
			t.Errorf("trial %d: DB player_black_id = %s, want %s (the goroutine that got a nil error)",
				trial, *game.PlayerBlackID, winnerID)
		}

		// In-memory session must agree with the DB — SetPlayerBlack is only
		// called on the success path in Manager.JoinGame. This confirms the
		// loser's goroutine never reached SetPlayerBlack despite racing.
		snap := session.CurrentStateSnapshot()
		if snap.PlayerBlackID != winnerID {
			t.Errorf("trial %d: in-memory session.PlayerBlackID = %q, want %q", trial, snap.PlayerBlackID, winnerID)
		}
	}
}
