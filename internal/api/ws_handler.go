package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/vedant-2701/chess/internal/auth"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// wsUpgrader configures the WebSocket upgrade handshake.
//
// CheckOrigin is permissive for Phase 1: there is no browser frontend yet,
// and ROADMAP.md's Phase 7 section explicitly calls out "WebSocket origin
// validation tightened on Go server" as in-scope once a real frontend origin
// exists to restrict to. Tightening this earlier would have nothing correct
// to check against.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WSHandler upgrades HTTP connections to WebSocket at GET /ws/game/{id} and
// bridges them into the game application layer.
//
// This is the only type in the codebase permitted to import both
// internal/ws (Connection, Registry) and internal/game (Manager) — see
// ARCHITECTURE.md's "internal/api" section and Dependency Graph.
// internal/ws itself has zero knowledge of games; internal/game has zero
// knowledge of HTTP. WSHandler is the bridge, and bridging is what the API
// layer is for.
//
// Originally specified as internal/ws/handler.go (PHASE_1.md Step 11). That
// location is a circular import: internal/game already imports internal/ws
// for *ws.Connection, so internal/ws cannot import internal/game back for
// *game.Manager. Relocated here; see the "Location correction" note in
// PHASE_1.md's Step 11 section.
type WSHandler struct {
	manager    *game.Manager
	wsRegistry *ws.Registry
	jwtSecret  string

	// ctx is a server-lifetime context (ADR-018), not a request-scoped one.
	// This is a documented, narrowly-scoped exception to
	// CODING_GUIDELINES.md §2 ("never store context in a struct"):
	// ReadLoop's onMessage/onClose callbacks — and therefore every
	// Manager.HandleMessage/HandleDisconnect call driven by this handler —
	// run for the lifetime of the WebSocket connection, which continues
	// well after ServeHTTP returns. There is no per-call context available
	// to thread through in that situation, only a connection lifetime and a
	// server lifetime; the server lifetime (cancelled on SIGTERM by
	// cmd/server/main.go at Step 13) is the correct scope. Do not use this
	// field as precedent for storing request-scoped contexts elsewhere.
	ctx context.Context
}

// NewWSHandler constructs a WSHandler.
//
// ctx must be a server-lifetime context per ADR-018 — created via
// context.WithCancel(context.Background()) in cmd/server/main.go (Step 13),
// with the returned cancel func invoked from the SIGTERM branch before
// wsRegistry.CloseAll(). Passing context.Background() directly is acceptable
// only until Step 13 wires real shutdown; it must not remain a permanent
// substitute for the real cancel func, since that would silently reopen
// Option A of ADR-018 (no shutdown cancellation reaches in-flight moves).
func NewWSHandler(ctx context.Context, manager *game.Manager, wsRegistry *ws.Registry, jwtSecret string) *WSHandler {
	return &WSHandler{
		manager:    manager,
		wsRegistry: wsRegistry,
		jwtSecret:  jwtSecret,
		ctx:        ctx,
	}
}

// ServeHTTP implements GET /ws/game/{id}?token=<playerToken>.
//
// Connection flow (PHASE_1.md "WebSocket Endpoint" section):
//  1. Verify token signature/expiry and extract claims.
//  2. Verify claims.GameID matches the :id URL parameter.
//  3. If either check fails: HTTP 401, no upgrade attempted.
//  4. Upgrade to WebSocket.
//  5. Hand off to game.Manager.HandleConnect. If the game session does not
//     exist (bad/stale gameID, or the game already ended and was cleaned up),
//     this fails post-upgrade — per spec, that case is a close-after-upgrade,
//     not a pre-upgrade 401, since session existence can only be checked
//     inside Manager against the live GameRegistry.
//  6. Start the connection's read/write/heartbeat goroutines and route
//     messages into game.Manager.
func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gameID := chi.URLParam(r, "id")
	if gameID == "" {
		http.Error(w, "missing game id in URL", http.StatusBadRequest)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "missing token query parameter")
		return
	}

	claims, err := auth.VerifyPlayerToken(token, h.jwtSecret)
	if err != nil {
		slog.Warn("WSHandler.ServeHTTP: token verification failed", "gameID", gameID, "error", err)
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "invalid or expired token")
		return
	}

	if claims.GameID != gameID {
		slog.Warn("WSHandler.ServeHTTP: token gameID does not match URL",
			"urlGameID", gameID, "tokenGameID", claims.GameID)
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "token does not match this game")
		return
	}

	color := store.Color(claims.Color)
	if color != store.ColorWhite && color != store.ColorBlack {
		slog.Error("WSHandler.ServeHTTP: token has an invalid color claim",
			"gameID", gameID, "color", claims.Color)
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "token has invalid claims")
		return
	}

	// Everything above this point can still respond with a normal HTTP error.
	// Upgrade commits the connection — no more pre-upgrade error responses
	// are possible after this succeeds.
	wsConn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote its own HTTP error response on failure.
		slog.Warn("WSHandler.ServeHTTP: upgrade failed", "gameID", gameID, "color", color, "error", err)
		return
	}

	connID := uuid.New().String() // v4: ephemeral, non-DB identifier — see CLAUDE.md UUID policy.
	conn := ws.NewConnection(connID, wsConn)
	h.wsRegistry.Register(connID, conn)

	if err := h.manager.HandleConnect(h.ctx, gameID, color, conn); err != nil {
		slog.Error("WSHandler.ServeHTTP: HandleConnect failed — closing connection",
			"gameID", gameID, "color", color, "connID", connID,
			"gameNotFound", errors.Is(err, game.ErrGameNotFound), "error", err)
		h.wsRegistry.Unregister(connID)
		// conn.Start has not been called, so WriteLoop is not running yet —
		// there is no queue-drain path available for a graceful WS close
		// frame, and writing directly to wsConn from this goroutine would
		// violate CODING_GUIDELINES.md §3 (writes only from the write-loop
		// goroutine). conn.Close() performs a plain TCP-level close instead;
		// the client's WebSocket onclose still fires, just without a
		// specific close code.
		conn.Close()
		return
	}

	conn.Start(
		func(raw []byte) {
			if err := h.manager.HandleMessage(h.ctx, gameID, color, raw); err != nil {
				slog.Error("WSHandler: HandleMessage failed",
					"gameID", gameID, "color", color, "connID", connID, "error", err)
			}
		},
		func() {
			h.manager.HandleDisconnect(gameID, color)
			h.wsRegistry.Unregister(connID)
		},
	)
}
