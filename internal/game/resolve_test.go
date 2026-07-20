//go:build integration

package game

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/goleak"

	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

const resolveTestJWTSecret = "resolve-test-secret"

// newTestManagerWithDirectory builds a Manager wired to a real RedisDirectory
// (testRedisClient, DB 1 — see testmain_test.go) under the given instanceID.
// Separate from manager_test.go's newTestManager, which deliberately passes
// directory=nil — ResolveGame is the only method that needs one, and most
// existing Manager tests have no reason to depend on Redis at all.
func newTestManagerWithDirectory(t *testing.T, instanceID string) *Manager {
	t.Helper()
	registry := NewGameRegistry()
	bus := NewLocalEventBus()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	processor := NewMoveProcessor(validator, gameStore, moveStore, bus)
	directory := NewRedisDirectory(testRedisClient)
	return NewManager(registry, processor, gameStore, moveStore, bus, resolveTestJWTSecret, validator, directory, instanceID)
}

func TestManager_ResolveGame_FreshClaim(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000001"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-a")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	connectToken, instanceLabel, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite)
	if err != nil {
		t.Fatalf("ResolveGame: %v", err)
	}
	if instanceLabel != "instance-a" {
		t.Errorf("instanceLabel: got %q, want %q", instanceLabel, "instance-a")
	}

	claims, err := auth.VerifyConnectToken(connectToken, resolveTestJWTSecret)
	if err != nil {
		t.Fatalf("VerifyConnectToken: %v", err)
	}
	if claims.GameID != session.ID {
		t.Errorf("claims.GameID: got %q, want %q", claims.GameID, session.ID)
	}
	if claims.UserID != whiteID {
		t.Errorf("claims.UserID: got %q, want %q", claims.UserID, whiteID)
	}
	if claims.Color != string(store.ColorWhite) {
		t.Errorf("claims.Color: got %q, want %q", claims.Color, store.ColorWhite)
	}
	if claims.InstanceLabel != "instance-a" {
		t.Errorf("claims.InstanceLabel: got %q, want %q", claims.InstanceLabel, "instance-a")
	}

	// Directory must actually reflect the claim.
	owner, ok, err := m.directory.GetOwner(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-a" {
		t.Errorf("directory owner: got %q ok=%v, want %q", owner, ok, "instance-a")
	}
}

func TestManager_ResolveGame_AlreadyOwnedAndAlive_NoReclaim(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000002"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-a")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	// First resolve claims ownership for instance-a but does NOT set its
	// liveness key (SetAlive is the heartbeat ticker's job, Step 6, not yet
	// built) — so a naive re-check would see the owner as not-alive and
	// attempt an unnecessary takeover. Set it explicitly here to isolate
	// exactly the "owner recorded AND alive" fast path this test targets.
	if _, _, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite); err != nil {
		t.Fatalf("first ResolveGame: %v", err)
	}
	if err := m.directory.SetAlive(ctx, "instance-a"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}

	_, instanceLabel, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite)
	if err != nil {
		t.Fatalf("second ResolveGame: %v", err)
	}
	if instanceLabel != "instance-a" {
		t.Errorf("instanceLabel: got %q, want %q (stable, no reclaim)", instanceLabel, "instance-a")
	}
}

func TestManager_ResolveGame_TakeoverFromDeadOwner(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000003"
	mustCreateUser(t, whiteID)

	ctx := context.Background()

	// Seed the directory as if "instance-dead" claimed this game and then
	// crashed without ever renewing — its ownership key exists, but its
	// liveness key was never set (or has already expired).
	directory := NewRedisDirectory(testRedisClient)
	gameUUID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	gameID := gameUUID.String()
	if err := store.NewGameStore(testPool).CreateGame(ctx, &store.Game{
		ID:            gameID,
		PlayerWhiteID: whiteID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   InitialTimeMs,
		BlackTimeMs:   InitialTimeMs,
	}); err != nil {
		t.Fatalf("CreateGame (DB): %v", err)
	}
	if claimed, err := directory.ClaimOwnership(ctx, gameID, "instance-dead", ""); err != nil || !claimed {
		t.Fatalf("seed ClaimOwnership: claimed=%v err=%v", claimed, err)
	}
	// Deliberately never call SetAlive("instance-dead") — simulates a crash
	// before the (not-yet-built) Step 6 heartbeat ticker ever ran.

	m := newTestManagerWithDirectory(t, "instance-b")

	_, instanceLabel, err := m.ResolveGame(ctx, gameID, whiteID, store.ColorWhite)
	if err != nil {
		t.Fatalf("ResolveGame: %v", err)
	}
	if instanceLabel != "instance-b" {
		t.Errorf("instanceLabel: got %q, want %q (takeover from dead owner)", instanceLabel, "instance-b")
	}

	owner, ok, err := m.directory.GetOwner(ctx, gameID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-b" {
		t.Errorf("directory owner after takeover: got %q ok=%v, want %q", owner, ok, "instance-b")
	}

	// The taking-over instance must have actually hydrated a live session,
	// not just won the ownership claim.
	if _, err := m.registry.Get(gameID); err != nil {
		t.Errorf("expected session to be hydrated locally after takeover: %v", err)
	}
}

func TestManager_ResolveGame_NonexistentGame_ReturnsNotFound(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000004"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-a")

	_, _, err := m.ResolveGame(ctx, uuid.NewString(), whiteID, store.ColorWhite)
	if err == nil {
		t.Fatal("expected error resolving a nonexistent game, got nil")
	}
	if !errors.Is(err, store.ErrGameNotFound) {
		t.Errorf("expected store.ErrGameNotFound, got: %v", err)
	}
}

// TestManager_ResolveGame_TerminalGame_Succeeds is the direct regression
// test for PHASE_2.md's Step 5/11 checklist requirement: resolving a game
// that has already ended (and whose session was already unregistered by
// finalizeGame, per normal Phase 1 behavior — see finalizeGame's doc
// comment) must succeed and hydrate a session carrying the correct terminal
// state, not error. This is the first legitimate reason in this codebase's
// history a terminal game's session needs to exist again after the fact.
//
// Also verifies the goroutine-leak gap this session's design work caught:
// hydrateGameSession must NOT subscribe the hydrated session to the
// EventBus for a terminal game, since a terminal game will never publish
// another GAME_OVER and the subscriber goroutine would otherwise leak
// forever (startEventSubscriber only exits on a GAME_OVER event or channel
// close). goleak.IgnoreCurrent() is required here per the standing pattern
// documented in CLAUDE.md's Known Sharp Edges — see
// TestWSHandler_GameOver_NoGoroutineLeaks for the reference implementation
// this mirrors.
func TestManager_ResolveGame_TerminalGame_Succeeds(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)

	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000005"
	const blackID = "40000000-0000-0000-0000-000000000006"
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-a")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := m.JoinGame(ctx, session.ID, blackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}
	// Both in-memory Transition AND the DB-level UpdateGameStatus are needed
	// here to correctly simulate what a real HandleConnect activation does
	// (RegisterConnection's atomic WAITING→ACTIVE transition, immediately
	// followed by the corresponding DB write in manager.go's HandleConnect).
	// A prior version of this test only did the in-memory Transition, which
	// left the DB row at WAITING_FOR_PLAYER — the subsequent handleResign
	// call's UpdateGameStatus(fromStatus=ACTIVE) would then silently fail its
	// compare-and-swap predicate (logged, not propagated, per handleResign's
	// own non-fatal error handling), leaving the DB never actually reaching
	// COMPLETED and defeating the entire point of this test.
	if err := session.Transition(store.GameStatusActive); err != nil {
		t.Fatalf("Transition to ACTIVE: %v", err)
	}
	if err := store.NewGameStore(testPool).UpdateGameStatus(ctx, session.ID, store.GameStatusWaiting, store.GameStatusActive, nil); err != nil {
		t.Fatalf("UpdateGameStatus to ACTIVE: %v", err)
	}

	// Drive the game to COMPLETED via resignation — this also runs
	// finalizeGame, unregistering the session, exactly as it would in real
	// gameplay (see manager.go's handleResign).
	m.handleResign(ctx, session, store.ColorWhite)

	if _, err := m.registry.Get(session.ID); err == nil {
		t.Fatal("precondition failed: session must be unregistered after resignation (finalizeGame)")
	}

	// Now resolve the same, already-terminal game — this is the actual
	// behavior under test.
	connectToken, instanceLabel, err := m.ResolveGame(ctx, session.ID, blackID, store.ColorBlack)
	if err != nil {
		t.Fatalf("ResolveGame on a terminal game: %v", err)
	}
	if instanceLabel != "instance-a" {
		t.Errorf("instanceLabel: got %q, want %q", instanceLabel, "instance-a")
	}

	claims, err := auth.VerifyConnectToken(connectToken, resolveTestJWTSecret)
	if err != nil {
		t.Fatalf("VerifyConnectToken: %v", err)
	}
	if claims.Color != string(store.ColorBlack) {
		t.Errorf("claims.Color: got %q, want %q", claims.Color, store.ColorBlack)
	}

	// The session must have been re-hydrated with the correct terminal
	// state.
	restored, err := m.registry.Get(session.ID)
	if err != nil {
		t.Fatalf("expected session to be re-hydrated after resolving a terminal game: %v", err)
	}
	snap := restored.CurrentStateSnapshot()
	if snap.Status != store.GameStatusCompleted {
		t.Errorf("restored session status: got %q, want COMPLETED", snap.Status)
	}
	if snap.Outcome == nil || *snap.Outcome != store.OutcomeBlack {
		t.Errorf("restored session outcome: got %v, want BLACK (White resigned)", snap.Outcome)
	}
	if snap.OutcomeReason == nil || *snap.OutcomeReason != store.OutcomeReasonResignation {
		t.Errorf("restored session outcomeReason: got %v, want RESIGNATION", snap.OutcomeReason)
	}
}

// TestManager_ResolveGame_ConcurrentResolves_SameInstanceLabel reproduces
// two players' independent resolve calls for the same brand-new game landing
// on the same instance concurrently — both must agree on the same
// instanceLabel, and GetOrHydrate's single-flight protection (Step 3) must
// prevent double-hydration.
func TestManager_ResolveGame_ConcurrentResolves_SameInstanceLabel(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "40000000-0000-0000-0000-000000000007"
	const blackID = "40000000-0000-0000-0000-000000000008"
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-a")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := m.JoinGame(ctx, session.ID, blackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}

	type result struct {
		instanceLabel string
		err           error
	}
	results := make(chan result, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, label, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite)
		results <- result{label, err}
	}()
	go func() {
		defer wg.Done()
		_, label, err := m.ResolveGame(ctx, session.ID, blackID, store.ColorBlack)
		results <- result{label, err}
	}()
	wg.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			t.Fatalf("ResolveGame: %v", r.err)
		}
		if r.instanceLabel != "instance-a" {
			t.Errorf("instanceLabel: got %q, want %q", r.instanceLabel, "instance-a")
		}
	}
}
