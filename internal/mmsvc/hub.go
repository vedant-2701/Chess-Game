package mmsvc

import (
	"log/slog"
	"sync"
)

// Event names and payload shapes for SSE pushes over GET /matchmaking/stream
// (PHASE_3.md's "Match Notification" section). Defined here, not in a
// future gRPC-server-specific file, because both this file's Stream handler
// and the not-yet-built MatchReportService gRPC server (a later Step 4
// checklist item) need the same two shapes — one to serialize, one to
// document what a caller of Hub.Notify should pass.
const (
	EventMatchFound        = "MATCH_FOUND"
	EventMatchmakingFailed = "MATCHMAKING_FAILED"
)

// matchFoundData mirrors PHASE_3.md's MATCH_FOUND payload exactly, and
// deliberately reuses existingGame's field set (not a new struct with
// different names) — same reasoning as existingGame's own doc comment:
// one client-side shape for "here is a game to connect to," used
// identically by the 409 response and this event.
type matchFoundData = existingGame

// matchmakingFailedData mirrors PHASE_3.md's MATCHMAKING_FAILED payload.
type matchmakingFailedData struct {
	Reason string `json:"reason"`
}

// Hub holds each connected player's outbound SSE event channel, keyed by
// userID, so a later event — MATCH_FOUND from the gRPC server,
// MATCHMAKING_FAILED from either the gRPC server or the queue-timeout sweep
// (both later Step 4 checklist items) — can be pushed to whichever open
// stream belongs to that player.
//
// In-memory, single-map, correct only at one matchmaking-service replica
// (M=1) — this is TD-P3-003, already tracked in PHASE_3.md, not a new
// limitation introduced here.
type Hub struct {
	// mu protects: conns
	mu    sync.RWMutex
	conns map[string]chan sseEvent
}

type sseEvent struct {
	event string
	data  any
}

// NewHub constructs an empty Hub.
func NewHub() *Hub {
	return &Hub{conns: make(map[string]chan sseEvent)}
}

// register creates a buffered event channel for userID, replacing any
// existing one. A stale prior connection (e.g. a browser tab reload) is
// simply overwritten — its old channel is abandoned; the old Stream
// goroutine still holding it exits on its own request context cancellation
// and never touches the map again (see unregister's guard below).
func (h *Hub) register(userID string) chan sseEvent {
	ch := make(chan sseEvent, 4)
	h.mu.Lock()
	h.conns[userID] = ch
	h.mu.Unlock()
	return ch
}

// unregister removes userID's channel, but only if it still points at ch —
// this guards against a newer register() (reconnect) racing a slower
// goroutine's deferred cleanup from an older, already-replaced connection;
// without the pointer-identity check, the cleanup from the OLD connection
// could run after the NEW one registered and incorrectly delete it.
func (h *Hub) unregister(userID string, ch chan sseEvent) {
	h.mu.Lock()
	if h.conns[userID] == ch {
		delete(h.conns, userID)
	}
	h.mu.Unlock()
}

// Notify pushes an event to userID's open stream, if one exists. Returns
// false if no stream is currently registered for userID — the client never
// connected, already disconnected, or is relying on GET /matchmaking/status
// as a fallback instead. That is a normal outcome PHASE_3.md's
// GET /matchmaking/status section already accounts for as the documented
// backstop, not an error condition this method needs to surface loudly.
//
// Non-blocking: a full channel (a slow/stuck reader — shouldn't happen with
// a buffer of 4 against at most two events ever sent per player, but
// defended anyway) drops the event rather than blocking the caller. Callers
// needing guaranteed delivery rely on GET /matchmaking/status, not this
// method's return value.
func (h *Hub) Notify(userID string, event string, data any) bool {
	h.mu.RLock()
	ch, ok := h.conns[userID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- sseEvent{event: event, data: data}:
		return true
	default:
		slog.Warn("Hub.Notify: event dropped, channel full", "userID", userID, "event", event)
		return false
	}
}
