package game

import (
	"fmt"
	"sync"
)

// GameRegistry is the in-memory index of all live game sessions on this server
// instance. It is the bridge between a gameID (from a JWT claim or URL param)
// and the *GameSession holding the live board and connections.
//
// In Phase 2 this registry remains local to the server instance; cross-instance
// game lookup is handled by the EventBus (Redis pub/sub).
type GameRegistry struct {
	// mu protects: sessions
	mu       sync.RWMutex
	sessions map[string]*GameSession
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