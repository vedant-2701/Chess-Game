//go:build integration

package game

import (
	"context"
	"testing"

	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// TestManager_HandleConnect_RegistryMiss_FallsBackToGetOrHydrate is
// PHASE_2.md Step 8's second explicit requirement: HandleConnect must
// recover a session that is not in the local registry (e.g. after this
// instance's own process restarted, or a fresh instance a game was never
// hydrated onto yet) by falling back to GetOrHydrate, rather than failing
// outright as the pre-Step-8 behavior did.
//
// Lives here (internal/game), not internal/api's ws_handler_test.go: the
// behavior under test is entirely inside Manager.HandleConnect, and testing
// it here means direct access to the private registry (to simulate a miss
// by unregistering a still-live, non-terminal session) without needing to
// add a test-only exported method to Manager's public API just to reach it
// from a different package.
func TestManager_HandleConnect_RegistryMiss_FallsBackToGetOrHydrate(t *testing.T) {
	truncateAll(t)

	const whiteID = "60000000-0000-0000-0000-000000000001"
	mustCreateUser(t, whiteID)

	ctx := context.Background()
	m := newTestManager(t)

	session, _, err := m.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	// Simulate "a fresh process with an empty registry, but a real row in
	// Postgres" by unregistering the still-live (non-terminal) session
	// directly — bypassing finalizeGame, which is only for terminal games.
	m.registry.Unregister(session.ID)
	if _, err := m.registry.Get(session.ID); err == nil {
		t.Fatal("precondition failed: session must actually be missing from the registry")
	}

	// A *ws.Connection with a nil underlying websocket — safe for HandleConnect
	// here because it only ever reaches Send (enqueues onto an in-memory
	// channel, never touches wsConn) via sendGameState, never Close/WriteLoop/
	// ReadLoop (this test never calls conn.Start or conn.Close). Mirrors
	// session_test.go's fakeConn helper exactly, including its documented
	// safety boundary — verified directly against internal/ws/connection.go's
	// source before relying on it here: Connection.Close() calls
	// c.wsConn.Close() unconditionally and would panic on a nil wsConn, so
	// this connection is deliberately never closed in this test.
	conn := ws.NewConnection("test-conn", nil)

	if err := m.HandleConnect(ctx, session.ID, store.ColorWhite, conn); err != nil {
		t.Fatalf("HandleConnect after registry miss: %v", err)
	}

	// The fallback must have actually hydrated and registered the session,
	// not just avoided erroring.
	restored, err := m.registry.Get(session.ID)
	if err != nil {
		t.Fatalf("expected session to be registered after HandleConnect's hydrate fallback: %v", err)
	}
	if restored.ID != session.ID {
		t.Errorf("restored session ID: got %q, want %q", restored.ID, session.ID)
	}
}
