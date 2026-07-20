package game

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// GameRegistry is the in-memory index of all live game sessions on this server
// instance. It is the bridge between a gameID (from a JWT claim or URL param)
// and the *GameSession holding the live board and connections.
//
// Under Phase 2's accepted design (DECISIONS_LOG_PHASE_2.md ADR-021), this
// registry remains local to the server instance permanently, not just for
// Phase 1 — there is never a second live GameSession for the same game on a
// different process. Cross-instance concerns are routing (which instance
// owns a game — internal/game/directory.go's RoutingDirectory), not lookup
// synchronization. LocalEventBus, not a Redis-backed one, remains the
// permanent EventBus implementation for the same reason (see ARCHITECTURE.md's
// corrected EventBus Interface section).
type GameRegistry struct {
	// mu protects: sessions
	mu       sync.RWMutex
	sessions map[string]*GameSession

	// hydrateGroup coalesces concurrent hydrate-on-miss calls for the same
	// gameID (PHASE_2.md Step 3, GetOrHydrate) so a miss triggers at most one
	// hydration regardless of how many callers race it — the same class of
	// double-registration race ADR-017 already closed once for first-connect,
	// recurring at the hydrate-on-miss boundary Phase 2 introduces.
	// singleflight.Group's zero value is ready to use; no initialization
	// needed in NewGameRegistry, and it is safe for concurrent use without
	// additional locking of its own.
	hydrateGroup singleflight.Group
}

// NewGameRegistry returns an empty GameRegistry ready for use.
func NewGameRegistry() *GameRegistry {
	return &GameRegistry{
		sessions: make(map[string]*GameSession),
	}
}

// Register adds session to the registry. If a session with the same ID already
// exists it is silently replaced — callers must not register duplicate IDs.
func (r *GameRegistry) Register(session *GameSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[session.ID] = session
}

// Get returns the GameSession for gameID. Returns ErrGameNotFound if no session
// is registered for that ID.
func (r *GameRegistry) Get(gameID string) (*GameSession, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[gameID]
	if !ok {
		return nil, fmt.Errorf("GameRegistry.Get gameID=%s: %w", gameID, ErrGameNotFound)
	}
	return s, nil
}

// Unregister removes the session for gameID. Safe to call if the ID is not
// present — it is a no-op in that case.
func (r *GameRegistry) Unregister(gameID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, gameID)
}

// AllActive returns a snapshot of all currently registered sessions.
// The snapshot-then-release pattern is used: the registry lock is released
// before any session method is called, so session methods can acquire their
// own locks without risking deadlock with the registry lock.
func (r *GameRegistry) AllActive() []*GameSession {
	r.mu.RLock()
	sessions := make([]*GameSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.RUnlock()
	return sessions
}

// hydrateContextTimeout bounds hydrateFn's execution inside GetOrHydrate.
// See DECISIONS_LOG_PHASE_2.md ADR-027 for why this exists at all and why
// 10s specifically — hydration is two DB round-trips (GetGame,
// GetMovesForGame) plus in-memory board replay, comparable in shape to
// HandleDisconnect's clock-persist write (ADR-019's 5s) but with more I/O,
// hence a slightly larger bound.
const hydrateContextTimeout = 10 * time.Second

// GetOrHydrate returns the GameSession for gameID, hydrating it via hydrateFn
// if it is not already registered. Concurrent callers racing a miss for the
// SAME gameID are coalesced via singleflight: hydrateFn runs at most once,
// and every caller — the one that triggered it and every other one that
// arrived during the same window — receives the identical *GameSession
// pointer. This closes the double-hydration race PHASE_2.md Step 3 requires,
// the same class of bug ADR-017 already closed once for first-connect (two
// players connecting concurrently to a brand-new game), now recurring at the
// hydrate-on-miss boundary Phase 2 introduces.
//
// hydrateFn performs the actual reconstruction (GetGame + GetMovesForGame +
// chess.GameFromMoves + NewGameSessionFromDB, per PHASE_2.md's Connection
// Flow step 3) and must NOT itself call Register — GetOrHydrate registers
// the winning result exactly once, still inside the singleflight critical
// section, so that a raw Get-based caller arriving between hydrateFn
// returning and registration cannot observe a still-missing session.
//
// IMPORTANT (ADR-027): hydrateFn does NOT receive ctx directly. Because
// singleflight runs the shared hydration under whichever caller happened to
// trigger it, using that caller's own context would let an unrelated,
// merely-unlucky caller's cancellation (their client disconnecting, their
// request timing out) abort hydration for every other caller silently
// piggybacking on the same result — exactly the ADR-019 failure shape,
// recurring at this new call site. hydrateFn instead runs under
// context.WithTimeout(context.WithoutCancel(ctx), hydrateContextTimeout):
// detached from any single caller's cancellation, but still bounded so a
// genuinely stuck DB call cannot hang every racing caller indefinitely.
func (r *GameRegistry) GetOrHydrate(ctx context.Context, gameID string, hydrateFn func(ctx context.Context) (*GameSession, error)) (*GameSession, error) {
	// Fast path: skip the singleflight machinery entirely when the session is
	// already registered — the overwhelming majority of calls, since a miss
	// only happens on first-ever connect or after failover/restart.
	if session, err := r.Get(gameID); err == nil {
		return session, nil
	}

	v, err, _ := r.hydrateGroup.Do(gameID, func() (interface{}, error) {
		// Re-check inside the singleflight critical section: another
		// goroutine may have already hydrated and registered gameID between
		// this call's fast-path Get above and this closure actually running.
		if session, err := r.Get(gameID); err == nil {
			return session, nil
		}

		hydrateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hydrateContextTimeout)
		defer cancel()

		session, err := hydrateFn(hydrateCtx)
		if err != nil {
			return nil, err
		}
		r.Register(session)
		return session, nil
	})
	if err != nil {
		return nil, fmt.Errorf("GameRegistry.GetOrHydrate gameID=%s: %w", gameID, err)
	}
	return v.(*GameSession), nil
}