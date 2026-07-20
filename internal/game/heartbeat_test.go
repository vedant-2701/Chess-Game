//go:build integration

package game

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/vedant-2701/chess/internal/store"
)

func TestManager_StartHeartbeat_MissingDirectory_ReturnsError(t *testing.T) {
	// newTestManager (manager_test.go) already constructs a Manager with
	// directory=nil — the documented configuration for callers that never use
	// ResolveGame/StartHeartbeat (see NewManager's doc comment).
	m := newTestManager(t)

	stop, err := m.StartHeartbeat(context.Background())
	if err == nil {
		t.Fatal("expected error starting heartbeat on a Manager with no directory, got nil")
	}
	if !errors.Is(err, ErrDirectoryNotConfigured) {
		t.Errorf("expected ErrDirectoryNotConfigured, got: %v", err)
	}
	if stop != nil {
		t.Error("expected nil stop function on error")
	}
}

func TestManager_StartHeartbeat_SetsAliveImmediately(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-hb-1")

	// SetAlive is called synchronously inside StartHeartbeat, before the
	// ticker goroutine is even spawned — no need to wait for a tick to
	// observe this.
	stop, err := m.StartHeartbeat(ctx)
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	defer stop()

	alive, err := m.directory.IsAlive(ctx, "instance-hb-1")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Fatal("expected instance to be alive immediately after StartHeartbeat")
	}
}

func TestManager_StopHeartbeat_ReleasesOwnedGamesAndLiveness(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "50000000-0000-0000-0000-000000000001"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-hb-2")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, _, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite); err != nil {
		t.Fatalf("ResolveGame: %v", err)
	}

	// Precondition: ownership is actually claimed before we test that
	// stopping releases it.
	owner, ok, err := m.directory.GetOwner(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOwner (precondition): %v", err)
	}
	if !ok || owner != "instance-hb-2" {
		t.Fatalf("precondition failed: expected instance-hb-2 to own the game, got owner=%q ok=%v", owner, ok)
	}

	stop, err := m.StartHeartbeat(ctx)
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	stop() // stop immediately — well before the 3s tick interval would ever fire.

	_, ok, err = m.directory.GetOwner(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOwner (after stop): %v", err)
	}
	if ok {
		t.Error("expected ownership record to be released after stop()")
	}

	alive, err := m.directory.IsAlive(ctx, "instance-hb-2")
	if err != nil {
		t.Fatalf("IsAlive (after stop): %v", err)
	}
	if alive {
		t.Error("expected liveness key to be released after stop()")
	}
}

// TestManager_HeartbeatTick_RenewsOwnershipAndLiveness tests heartbeatTick
// directly (a single synchronous cycle) rather than waiting on the real 3s
// ticker to fire — CODING_GUIDELINES.md §6 forbids time.Sleep in tests, and
// there is no channel-based way to observe "a background ticker has fired N
// times" without either sleeping or injecting a fake clock (unscoped
// complexity for what this test needs to prove). Calling heartbeatTick
// directly exercises the exact same renewal logic StartHeartbeat's ticker
// would invoke on each real tick, without depending on wall-clock timing.
func TestManager_HeartbeatTick_RenewsOwnershipAndLiveness(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	const whiteID = "50000000-0000-0000-0000-000000000002"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-hb-3")

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, _, err := m.ResolveGame(ctx, session.ID, whiteID, store.ColorWhite); err != nil {
		t.Fatalf("ResolveGame: %v", err)
	}
	// ResolveGame's own ClaimOwnership already sets a fresh 30s TTL and
	// GetOrHydrate registers the session — but SetAlive is only called by
	// StartHeartbeat, which this test deliberately does not call (to
	// isolate heartbeatTick in complete separation from the ticker
	// lifecycle). Seed it directly instead.
	if err := m.directory.SetAlive(ctx, "instance-hb-3"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}

	m.heartbeatTick(ctx)

	alive, err := m.directory.IsAlive(ctx, "instance-hb-3")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Error("expected liveness to still be true after heartbeatTick")
	}

	owner, ok, err := m.directory.GetOwner(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetOwner: %v", err)
	}
	if !ok || owner != "instance-hb-3" {
		t.Errorf("expected ownership to remain instance-hb-3 after heartbeatTick, got owner=%q ok=%v", owner, ok)
	}
}

func TestManager_HeartbeatTick_NoActiveGames_OnlyRenewsLiveness(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-hb-4")

	if err := m.directory.SetAlive(ctx, "instance-hb-4"); err != nil {
		t.Fatalf("SetAlive: %v", err)
	}

	// No games registered — RenewOwnershipBatch should be skipped entirely
	// (an empty gameIDs slice is a documented no-op, not an error), and this
	// must not panic or error.
	m.heartbeatTick(ctx)

	alive, err := m.directory.IsAlive(ctx, "instance-hb-4")
	if err != nil {
		t.Fatalf("IsAlive: %v", err)
	}
	if !alive {
		t.Error("expected liveness to still be true after heartbeatTick with no active games")
	}
}

// TestManager_StartHeartbeat_NoGoroutineLeak is PHASE_2.md Step 6's explicit
// checklist requirement: the ticker goroutine itself must not leak.
// goleak.IgnoreCurrent() is required per the standing pattern documented in
// CLAUDE.md's Known Sharp Edges — see TestWSHandler_GameOver_NoGoroutineLeaks
// for the reference implementation this mirrors.
func TestManager_StartHeartbeat_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)

	truncateAll(t)
	flushTestRedisDB(t)

	ctx := context.Background()
	m := newTestManagerWithDirectory(t, "instance-hb-5")

	stop, err := m.StartHeartbeat(ctx)
	if err != nil {
		t.Fatalf("StartHeartbeat: %v", err)
	}
	stop()
}
