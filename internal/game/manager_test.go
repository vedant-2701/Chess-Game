//go:build integration

package game

import (
	"context"
	"errors"
	"testing"

	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

const (
	mgrTestWhiteID = "10000000-0000-0000-0000-000000000001"
	mgrTestBlackID = "10000000-0000-0000-0000-000000000002"
)

// newTestManager builds a Manager wired to testPool with a fresh registry and
// LocalEventBus per call, so tests do not share state through a package-level
// Manager.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	registry := NewGameRegistry()
	bus := NewLocalEventBus()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	processor := NewMoveProcessor(validator, gameStore, moveStore, bus)

	return NewManager(registry, processor, gameStore, moveStore, bus, "test-jwt-secret-not-for-prod", validator)
}

// --- CreateGame ---------------------------------------------------------

func TestManager_CreateGame_PersistsAndRegisters(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session, token, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if session == nil {
		t.Fatal("CreateGame returned nil session")
	}
	if token == "" {
		t.Fatal("CreateGame returned empty token")
	}

	// Session must be registered and retrievable.
	got, err := m.registry.Get(session.ID)
	if err != nil {
		t.Fatalf("registry.Get(%s): %v", session.ID, err)
	}
	if got != session {
		t.Error("registry.Get returned a different session than CreateGame returned")
	}

	// In-memory session state.
	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusWaiting {
		t.Errorf("session status: got %q, want WAITING_FOR_PLAYER", snap.Status)
	}
	if snap.PlayerWhiteID != mgrTestWhiteID {
		t.Errorf("session.PlayerWhiteID: got %q, want %q", snap.PlayerWhiteID, mgrTestWhiteID)
	}
	if snap.PlayerBlackID != "" {
		t.Errorf("session.PlayerBlackID: got %q, want empty", snap.PlayerBlackID)
	}
	if snap.CurrentFEN != store.StartingFEN {
		t.Errorf("session.CurrentFEN: got %q, want starting position", snap.CurrentFEN)
	}

	// DB state.
	game, err := store.NewGameStore(testPool).GetGame(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != store.GameStatusWaiting {
		t.Errorf("DB game.Status: got %q, want WAITING_FOR_PLAYER", game.Status)
	}
	if game.PlayerWhiteID != mgrTestWhiteID {
		t.Errorf("DB game.PlayerWhiteID: got %q, want %q", game.PlayerWhiteID, mgrTestWhiteID)
	}
	if game.PlayerBlackID != nil {
		t.Errorf("DB game.PlayerBlackID: got %v, want nil", game.PlayerBlackID)
	}
	if game.WhiteTimeMs != InitialTimeMs || game.BlackTimeMs != InitialTimeMs {
		t.Errorf("DB clocks: got white=%d black=%d, want both %d",
			game.WhiteTimeMs, game.BlackTimeMs, InitialTimeMs)
	}

	// Token must verify and carry the expected claims.
	claims, err := auth.VerifyPlayerToken(token, "test-jwt-secret-not-for-prod")
	if err != nil {
		t.Fatalf("VerifyPlayerToken: %v", err)
	}
	if claims.GameID != session.ID {
		t.Errorf("token GameID: got %q, want %q", claims.GameID, session.ID)
	}
	if claims.UserID != mgrTestWhiteID {
		t.Errorf("token UserID: got %q, want %q", claims.UserID, mgrTestWhiteID)
	}
	if claims.Color != string(store.ColorWhite) {
		t.Errorf("token Color: got %q, want WHITE", claims.Color)
	}
}

func TestManager_CreateGame_GeneratesDistinctGameIDsAcrossCalls(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session1, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame (1): %v", err)
	}
	session2, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame (2): %v", err)
	}

	if session1.ID == session2.ID {
		t.Fatal("two CreateGame calls produced the same gameID")
	}

	// Both must be independently retrievable from the registry.
	if _, err := m.registry.Get(session1.ID); err != nil {
		t.Errorf("registry.Get(session1.ID): %v", err)
	}
	if _, err := m.registry.Get(session2.ID); err != nil {
		t.Errorf("registry.Get(session2.ID): %v", err)
	}
}

// --- JoinGame ------------------------------------------------------------

func TestManager_JoinGame_UpdatesSessionAndDB(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	token, err := m.JoinGame(ctx, session.ID, mgrTestBlackID)
	if err != nil {
		t.Fatalf("JoinGame: %v", err)
	}
	if token == "" {
		t.Fatal("JoinGame returned empty token")
	}

	claims, err := auth.VerifyPlayerToken(token, "test-jwt-secret-not-for-prod")
	if err != nil {
		t.Fatalf("VerifyPlayerToken: %v", err)
	}
	if claims.Color != string(store.ColorBlack) {
		t.Errorf("token Color: got %q, want BLACK", claims.Color)
	}
	if claims.UserID != mgrTestBlackID {
		t.Errorf("token UserID: got %q, want %q", claims.UserID, mgrTestBlackID)
	}

	// Session reflects the joined player. Note: JoinGame does not transition
	// status to ACTIVE — per PHASE_1.md, that only happens when Black's
	// WebSocket connects (Manager.HandleConnect), not on the HTTP join call.
	snap := session.CurrentStateSnapshot()
	if snap.PlayerBlackID != mgrTestBlackID {
		t.Errorf("session.PlayerBlackID: got %q, want %q", snap.PlayerBlackID, mgrTestBlackID)
	}
	if snap.Status != store.GameStatusWaiting {
		t.Errorf("session status after JoinGame: got %q, want still WAITING_FOR_PLAYER", snap.Status)
	}

	// DB state.
	game, err := store.NewGameStore(testPool).GetGame(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.PlayerBlackID == nil || *game.PlayerBlackID != mgrTestBlackID {
		t.Errorf("DB game.PlayerBlackID: got %v, want %q", game.PlayerBlackID, mgrTestBlackID)
	}
}

func TestManager_JoinGame_RejectsSelfPlay(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	_, err = m.JoinGame(ctx, session.ID, mgrTestWhiteID)
	if err == nil {
		t.Fatal("expected error for self-play, got nil")
	}
	if !errors.Is(err, ErrSelfPlay) {
		t.Errorf("expected ErrSelfPlay, got: %v", err)
	}

	// Black slot must remain unset.
	snap := session.CurrentStateSnapshot()
	if snap.PlayerBlackID != "" {
		t.Errorf("session.PlayerBlackID after rejected self-play: got %q, want empty", snap.PlayerBlackID)
	}
}

func TestManager_JoinGame_RejectsWhenAlreadyJoined(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := m.JoinGame(ctx, session.ID, mgrTestBlackID); err != nil {
		t.Fatalf("first JoinGame: %v", err)
	}

	// A second, different user attempts to join the same already-full game.
	const thirdUserID = "10000000-0000-0000-0000-000000000003"
	mustCreateUser(t, thirdUserID)

	_, err = m.JoinGame(ctx, session.ID, thirdUserID)
	if err == nil {
		t.Fatal("expected error joining an already-full game, got nil")
	}
	if !errors.Is(err, ErrGameNotJoinable) {
		t.Errorf("expected ErrGameNotJoinable, got: %v", err)
	}
}

func TestManager_JoinGame_NonexistentGame_ReturnsError(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestBlackID)

	ctx := context.Background()
	m := newTestManager(t)

	_, err := m.JoinGame(ctx, "00000000-0000-0000-0000-000000000000", mgrTestBlackID)
	if err == nil {
		t.Fatal("expected error joining a nonexistent game, got nil")
	}
}

// --- RestoreActiveGames ---------------------------------------------------

// mustCreateActiveGameDBOnly inserts a game directly into the DB at ACTIVE
// status with the given moves applied, WITHOUT registering an in-memory
// GameSession — simulating the state left behind by a server process that
// has exited, which RestoreActiveGames must reconstruct purely from the DB.
func mustCreateActiveGameDBOnly(t *testing.T, gameID, whiteID, blackID string, sans []string) {
	t.Helper()
	ctx := context.Background()
	gs := store.NewGameStore(testPool)
	ms := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()

	if err := gs.CreateGame(ctx, &store.Game{
		ID:            gameID,
		PlayerWhiteID: whiteID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   InitialTimeMs,
		BlackTimeMs:   InitialTimeMs,
	}); err != nil {
		t.Fatalf("mustCreateActiveGameDBOnly CreateGame: %v", err)
	}
	if err := gs.UpdatePlayerBlack(ctx, gameID, blackID); err != nil {
		t.Fatalf("mustCreateActiveGameDBOnly UpdatePlayerBlack: %v", err)
	}
	if err := gs.UpdateGameStatus(ctx, gameID, store.GameStatusActive, nil); err != nil {
		t.Fatalf("mustCreateActiveGameDBOnly UpdateGameStatus: %v", err)
	}

	board := internalchess.NewGame()
	colors := []store.Color{store.ColorWhite, store.ColorBlack}
	var fenAfter string
	for i, san := range sans {
		if err := validator.ValidateMove(board, san); err != nil {
			t.Fatalf("mustCreateActiveGameDBOnly: ValidateMove %q: %v", san, err)
		}
		var err error
		fenAfter, err = internalchess.ComputeFENAfterMove(board, san)
		if err != nil {
			t.Fatalf("mustCreateActiveGameDBOnly: ComputeFENAfterMove %q: %v", san, err)
		}
		if err := ms.SaveMove(ctx, &store.Move{
			GameID:     gameID,
			MoveNumber: i + 1,
			Color:      colors[i%2],
			SAN:        san,
			FENAfter:   fenAfter,
		}); err != nil {
			t.Fatalf("mustCreateActiveGameDBOnly: SaveMove %q: %v", san, err)
		}
		if err := validator.ApplyMove(board, san); err != nil {
			t.Fatalf("mustCreateActiveGameDBOnly: ApplyMove %q: %v", san, err)
		}
	}
	if fenAfter != "" {
		if err := gs.UpdateCurrentFEN(ctx, gameID, fenAfter); err != nil {
			t.Fatalf("mustCreateActiveGameDBOnly: UpdateCurrentFEN: %v", err)
		}
	}
}

func TestManager_RestoreActiveGames_HydratesInProgressGame(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	const gameID = "20000000-0000-0000-0000-000000000001"
	mustCreateActiveGameDBOnly(t, gameID, mgrTestWhiteID, mgrTestBlackID, []string{"e4", "e5", "Nf3"})

	ctx := context.Background()
	m := newTestManager(t)

	if err := m.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames: %v", err)
	}

	session, err := m.registry.Get(gameID)
	if err != nil {
		t.Fatalf("registry.Get(%s) after restore: %v", gameID, err)
	}

	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive {
		t.Errorf("restored session status: got %q, want ACTIVE", snap.Status)
	}
	if len(snap.Moves) != 3 {
		t.Errorf("restored session moves: got %d, want 3", len(snap.Moves))
	}
	if snap.Turn != store.ColorBlack {
		t.Errorf("restored session turn: got %q, want BLACK (after 3 half-moves)", snap.Turn)
	}
	if snap.PlayerWhiteID != mgrTestWhiteID {
		t.Errorf("restored session PlayerWhiteID: got %q, want %q", snap.PlayerWhiteID, mgrTestWhiteID)
	}
	if snap.PlayerBlackID != mgrTestBlackID {
		t.Errorf("restored session PlayerBlackID: got %q, want %q", snap.PlayerBlackID, mgrTestBlackID)
	}

	// Clock must be hydrated but NOT started (no live connections yet).
	if session.clock.IsStarted() {
		t.Error("restored session clock.IsStarted() = true, want false until HandleConnect")
	}
}

func TestManager_RestoreActiveGames_IgnoresStaleCurrentFEN(t *testing.T) {
	// Regression test for the documented Known Sharp Edge: RestoreActiveGames
	// must reconstruct the board via GameFromMoves (the moves table), never
	// via GameFromFEN(games.current_fen) — current_fen can go stale per the
	// non-fatal UpdateCurrentFEN failure mode in ProcessMove (Step 8).
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	const gameID = "20000000-0000-0000-0000-000000000002"
	mustCreateActiveGameDBOnly(t, gameID, mgrTestWhiteID, mgrTestBlackID, []string{"e4", "e5"})

	// Deliberately corrupt current_fen to a value inconsistent with the
	// actual moves table, simulating the documented non-fatal failure mode.
	ctx := context.Background()
	gs := store.NewGameStore(testPool)
	if err := gs.UpdateCurrentFEN(ctx, gameID, store.StartingFEN); err != nil {
		t.Fatalf("corrupt current_fen setup: %v", err)
	}

	m := newTestManager(t)
	if err := m.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames: %v", err)
	}

	session, err := m.registry.Get(gameID)
	if err != nil {
		t.Fatalf("registry.Get(%s) after restore: %v", gameID, err)
	}

	// If RestoreActiveGames had used the corrupted current_fen, the board
	// would be at the starting position with 0 moves. It must instead reflect
	// the true 2-move history from the moves table.
	snap := session.CurrentStateSnapshot()
	if snap.CurrentFEN == store.StartingFEN {
		t.Error("restored session used stale current_fen instead of replaying moves table")
	}
	if len(snap.Moves) != 2 {
		t.Errorf("restored session moves: got %d, want 2 (ignoring corrupted current_fen)", len(snap.Moves))
	}
}

func TestManager_RestoreActiveGames_CorrectsZombieActiveGame(t *testing.T) {
	// Regression test for the documented Known Sharp Edge: if handleGameOver
	// published GAME_OVER but the subsequent UpdateGameStatus DB write failed,
	// the DB can show ACTIVE for a game that is actually over (checkmate
	// reachable by replaying its moves). RestoreActiveGames must detect this,
	// correct the DB to COMPLETED, and NOT add the session to the registry.
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	const gameID = "20000000-0000-0000-0000-000000000003"
	// Scholar's mate — White delivers checkmate on the 4th move.
	mustCreateActiveGameDBOnly(t, gameID, mgrTestWhiteID, mgrTestBlackID,
		[]string{"e4", "e5", "Qh5", "Nc6", "Bc4", "Nf6", "Qxf7"})

	// Status was left ACTIVE in the DB despite the board being checkmate —
	// mustCreateActiveGameDBOnly only ever sets ACTIVE, it never calls
	// UpdateGameStatus(COMPLETED), so this reproduces the zombie condition
	// directly without needing to fake a failed DB write.

	ctx := context.Background()
	m := newTestManager(t)

	if err := m.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames: %v", err)
	}

	// Must NOT be added to the registry — a checkmated game is not joinable/live.
	if _, err := m.registry.Get(gameID); err == nil {
		t.Error("zombie ACTIVE game was added to the registry, want excluded")
	}

	// DB must be corrected to COMPLETED with the right outcome.
	game, err := store.NewGameStore(testPool).GetGame(ctx, gameID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if game.Status != store.GameStatusCompleted {
		t.Errorf("DB game.Status after restore: got %q, want COMPLETED", game.Status)
	}
	if game.Outcome == nil || *game.Outcome != store.OutcomeWhite {
		t.Errorf("DB game.Outcome after restore: got %v, want WHITE", game.Outcome)
	}
	if game.OutcomeReason == nil || *game.OutcomeReason != store.OutcomeReasonCheckmate {
		t.Errorf("DB game.OutcomeReason after restore: got %v, want CHECKMATE", game.OutcomeReason)
	}
}

func TestManager_RestoreActiveGames_RestoresWaitingForPlayerGame(t *testing.T) {
	// A game that was created but never reached ACTIVE (White created it,
	// server restarted before Black connected) is still returned by
	// GetActiveGames (per its WAITING_FOR_PLAYER inclusion) and must be
	// restored without error and without being misclassified as a zombie.
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	gameID := session.ID

	// Simulate a fresh process: new Manager, new (empty) registry.
	m2 := newTestManager(t)
	if err := m2.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames: %v", err)
	}

	restored, err := m2.registry.Get(gameID)
	if err != nil {
		t.Fatalf("registry.Get(%s) after restore: %v", gameID, err)
	}
	snap := restored.CurrentStateSnapshot()
	if snap.Status != store.GameStatusWaiting {
		t.Errorf("restored WAITING game status: got %q, want WAITING_FOR_PLAYER", snap.Status)
	}
}

func TestManager_RestoreActiveGames_SkipsCompletedGames(t *testing.T) {
	// GetActiveGames only returns ACTIVE/WAITING games; a genuinely COMPLETED
	// game (correctly persisted, not a zombie) must not be restored at all.
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	const gameID = "20000000-0000-0000-0000-000000000004"
	mustCreateActiveGameDBOnly(t, gameID, mgrTestWhiteID, mgrTestBlackID, []string{"e4"})

	ctx := context.Background()
	gs := store.NewGameStore(testPool)
	winner := store.OutcomeWhite
	reason := store.OutcomeReasonResignation
	if err := gs.UpdateGameStatus(ctx, gameID, store.GameStatusCompleted, &store.GameOutcome{
		Outcome: winner,
		Reason:  reason,
	}); err != nil {
		t.Fatalf("UpdateGameStatus to COMPLETED: %v", err)
	}

	m := newTestManager(t)
	if err := m.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames: %v", err)
	}

	if _, err := m.registry.Get(gameID); err == nil {
		t.Error("a genuinely COMPLETED game was restored into the registry, want excluded")
	}
}

// --- HandleDisconnect ------------------------------------------------

// TestManager_HandleDisconnect_PersistsClockState is a regression test for
// this session's fix: HandleDisconnect previously only called
// session.clock.Pause() (in-memory only) and never wrote the paused reading
// to the database, leaving the DB holding whatever was written after the
// game's last move. A player who disconnects mid-turn and is then caught by
// a hard kill -9 (no graceful shutdown) would resume with extra time that
// was never actually theirs.
//
// To make this deterministic without any time.Sleep (forbidden by
// CODING_GUIDELINES.md §6), the test seeds the DB with a distinctive "stale"
// sentinel value that is neither InitialTimeMs nor the live clock's actual
// reading, then asserts the persisted value moves to the live clock's
// reading, not that it changes by some expected amount over elapsed time.
// A before/after comparison using only InitialTimeMs would not catch a
// regression here, since near-zero wall-clock time elapses during the test
// and the "before" and "after" values would look identical either way if the
// persist call were silently removed.
func TestManager_HandleDisconnect_PersistsClockState(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := m.JoinGame(ctx, session.ID, mgrTestBlackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}
	if err := session.Transition(store.GameStatusActive); err != nil {
		t.Fatalf("Transition to ACTIVE: %v", err)
	}

	// Seed a distinctive, obviously-wrong DB value before disconnecting.
	gs := store.NewGameStore(testPool)
	const staleMs = 999999
	if err := gs.UpdateClocks(ctx, session.ID, staleMs, staleMs); err != nil {
		t.Fatalf("seed stale clock: %v", err)
	}

	// Swap in a clock with known, distinct-from-stale remaining times and
	// start it for White. Direct field access is legitimate here — same
	// package — since GameSession exposes no "ReplaceClock" method; nothing
	// in the public API needs one outside tests.
	const liveWhiteMs, liveBlackMs int64 = 500000, 480000
	session.clock = NewClockWithTimes(liveWhiteMs, liveBlackMs)
	session.clock.Start(store.ColorWhite)
	t.Cleanup(session.clock.Stop) // avoid leaking the Clock's background goroutine

	m.HandleDisconnect(ctx, session.ID, store.ColorWhite)

	game, err := gs.GetGame(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetGame after disconnect: %v", err)
	}

	if game.WhiteTimeMs == staleMs || game.BlackTimeMs == staleMs {
		t.Fatalf("DB clocks still show the stale sentinel after disconnect: white=%d black=%d — persist did not happen",
			game.WhiteTimeMs, game.BlackTimeMs)
	}

	// Black was never the active color, so its remaining time must be exactly
	// unchanged — no wall-clock dependency in this assertion.
	if game.BlackTimeMs != liveBlackMs {
		t.Errorf("DB BlackTimeMs after disconnect: got %d, want exactly %d (inactive color, untouched by elapsed time)",
			game.BlackTimeMs, liveBlackMs)
	}

	// White was the active, disconnecting color: Pause() deducts real elapsed
	// time since Start(), so allow a generous bound for test execution
	// overhead rather than asserting exact equality.
	if game.WhiteTimeMs > liveWhiteMs {
		t.Errorf("DB WhiteTimeMs after disconnect: got %d, want <= %d (Pause must not increase remaining time)",
			game.WhiteTimeMs, liveWhiteMs)
	}
	const maxElapsedToleranceMs = 5000
	if liveWhiteMs-game.WhiteTimeMs > maxElapsedToleranceMs {
		t.Errorf("DB WhiteTimeMs after disconnect: got %d, more than %dms below the live reading %d — suspicious for a test with no real gameplay delay",
			game.WhiteTimeMs, maxElapsedToleranceMs, liveWhiteMs)
	}

	// In-memory session state must also reflect the persisted values
	// (HandleDisconnect calls session.UpdateClocks alongside the DB write).
	snap := session.CurrentStateSnapshot()
	if snap.WhiteTimeMs != game.WhiteTimeMs || snap.BlackTimeMs != game.BlackTimeMs {
		t.Errorf("in-memory session clocks (white=%d black=%d) do not match persisted DB clocks (white=%d black=%d)",
			snap.WhiteTimeMs, snap.BlackTimeMs, game.WhiteTimeMs, game.BlackTimeMs)
	}
}

// TestManager_HandleDisconnect_ClockNotStarted_NoOp covers the defensive
// branch: a player disconnecting before the clock has ever been started
// (e.g. White created a game and disconnected again before Black joined)
// must not attempt to persist clock state at all — IsStarted() gates the
// entire block. This also guards against a nil-handling regression if the
// gating condition were ever removed.
func TestManager_HandleDisconnect_ClockNotStarted_NoOp(t *testing.T) {
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, mgrTestWhiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	if session.clock.IsStarted() {
		t.Fatal("precondition failed: freshly created session's clock must not be started yet")
	}

	// Must not panic and must not touch the DB clock columns.
	m.HandleDisconnect(ctx, session.ID, store.ColorWhite)

	gs := store.NewGameStore(testPool)
	game, err := gs.GetGame(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetGame after disconnect: %v", err)
	}
	if game.WhiteTimeMs != InitialTimeMs || game.BlackTimeMs != InitialTimeMs {
		t.Errorf("DB clocks changed despite clock never having started: white=%d black=%d, want both %d",
			game.WhiteTimeMs, game.BlackTimeMs, InitialTimeMs)
	}
}

func TestManager_RestoreActiveGames_MultipleGamesIndependentFailureIsolation(t *testing.T) {
	// One game with an unreplayable move sequence must not prevent other,
	// valid games from being restored. RestoreActiveGames logs and skips
	// individual failures per its documented contract.
	truncateAll(t)
	mustCreateUser(t, mgrTestWhiteID)
	mustCreateUser(t, mgrTestBlackID)

	const goodGameID = "20000000-0000-0000-0000-000000000005"
	const badGameID = "20000000-0000-0000-0000-000000000006"

	mustCreateActiveGameDBOnly(t, goodGameID, mgrTestWhiteID, mgrTestBlackID, []string{"e4", "e5"})

	// Insert a second game directly with a move row that is illegal from the
	// starting position, so GameFromMoves fails to replay it.
	ctx := context.Background()
	gs := store.NewGameStore(testPool)
	ms := store.NewMoveStore(testPool)
	if err := gs.CreateGame(ctx, &store.Game{
		ID:            badGameID,
		PlayerWhiteID: mgrTestWhiteID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   InitialTimeMs,
		BlackTimeMs:   InitialTimeMs,
	}); err != nil {
		t.Fatalf("CreateGame badGame: %v", err)
	}
	if err := gs.UpdatePlayerBlack(ctx, badGameID, mgrTestBlackID); err != nil {
		t.Fatalf("UpdatePlayerBlack badGame: %v", err)
	}
	if err := gs.UpdateGameStatus(ctx, badGameID, store.GameStatusActive, nil); err != nil {
		t.Fatalf("UpdateGameStatus badGame: %v", err)
	}
	if err := ms.SaveMove(ctx, &store.Move{
		GameID:     badGameID,
		MoveNumber: 1,
		Color:      store.ColorWhite,
		SAN:        "e5", // illegal as White's first move
		FENAfter:   store.StartingFEN,
	}); err != nil {
		t.Fatalf("SaveMove illegal move: %v", err)
	}

	m := newTestManager(t)
	if err := m.RestoreActiveGames(ctx); err != nil {
		t.Fatalf("RestoreActiveGames returned an error (should isolate per-game failures): %v", err)
	}

	if _, err := m.registry.Get(goodGameID); err != nil {
		t.Errorf("good game was not restored despite an unrelated bad game: %v", err)
	}
	if _, err := m.registry.Get(badGameID); err == nil {
		t.Error("unreplayable game was incorrectly added to the registry")
	}
}