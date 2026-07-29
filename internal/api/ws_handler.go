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
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return origin == "http://"+r.Host || origin == "https://"+r.Host
	},
}

// WSHandler upgrades HTTP connections to WebSocket at the masked
// GET /connect/{instanceLabel} URL (PHASE_2.md Step 8, ADR-022) and bridges
// them into the game application layer.
//
// This is the only type in the codebase permitted to import both
// internal/ws (Connection, Registry) and internal/game (Manager) — see
// ARCHITECTURE.md's "internal/api" section and Dependency Graph.
// internal/ws itself has zero knowledge of games; internal/game has zero
// knowledge of HTTP. WSHandler is the bridge, and bridging is what the API
// layer is for.
//
// Route history: originally GET /ws/game/{id}?token=<playerToken>
// (PHASE_1.md Step 11). Replaced entirely, not kept alongside the new route,
// by PHASE_2.md Step 8's resolve-then-connect flow — ADR-022 is explicit
// that every connect or reconnect, first or subsequent, uses the identical
// resolve-then-connect path with no special-casing; keeping the old
// PlayerClaims-direct-WS path alive as a second, parallel connect mechanism
// would be exactly the kind of topology-specific duplication this project
// has repeatedly declined elsewhere (see DECISIONS_LOG_PHASE_2.md ADR-024's
// identical reasoning for RestoreActiveGames). A client must now call
// GET /games/:id/resolve first to obtain a ConnectClaims token before
// dialing this endpoint at all — see internal/api/game_handler.go's Resolve
// handler.
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
	// cmd/server/main.go) is the correct scope. Do not use this field as
	// precedent for storing request-scoped contexts elsewhere.
	ctx context.Context
}

// NewWSHandler constructs a WSHandler.
//
// ctx must be a server-lifetime context per ADR-018 — created via
// context.WithCancel(context.Background()) in cmd/server/main.go, with the
// returned cancel func invoked from the SIGTERM branch before
// wsRegistry.CloseAll().
func NewWSHandler(ctx context.Context, manager *game.Manager, wsRegistry *ws.Registry, jwtSecret string) *WSHandler {
	return &WSHandler{
		manager:    manager,
		wsRegistry: wsRegistry,
		jwtSecret:  jwtSecret,
		ctx:        ctx,
	}
}

// ServeHTTP implements GET /connect/{instanceLabel}?token=<connectToken>
// (PHASE_2.md Step 8).
//
// Connection flow:
//  1. Verify the ConnectClaims token's signature/expiry and extract claims.
//     gameID, userID, and color all come from the verified token, NOT from
//     the URL — the masked URL deliberately carries no gameID segment (see
//     ADR-022's "masked WS URL wss://mygame.com/connect/{owner}"); it exists
//     only for nginx's Edge Proxy to mechanically dereference {instanceLabel}
//     to the correct backend before this handler ever runs.
//  2. Verify claims.InstanceLabel matches the :instanceLabel URL parameter —
//     catches a token minted for a different instance than the one it's
//     being presented to (e.g. a stale/tampered masked URL), mirroring the
//     same "URL vs. token claim" integrity check the old PlayerClaims path
//     already did for gameID.
//  3. An expired ConnectClaims token gets its own distinct error code
//     (ErrCodeConnectTokenExpired) and response message telling the client
//     to call resolve again — this is PHASE_2.md Step 5's originally-deferred
//     test, now implementable because this method exists to reject against.
//     ConnectClaims' 10s TTL (ADR-022) makes this the expected outcome of a
//     slow client, not evidence of a bug — logged at Info, not Warn/Error.
//  4. Upgrade to WebSocket.
//  5. Hand off to game.Manager.HandleConnect, which (as of this same step)
//     falls back to GetOrHydrate on a local registry miss — see
//     HandleConnect's doc comment and ADR-024.
//  6. Start the connection's read/write/heartbeat goroutines and route
//     messages into game.Manager.
func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	instanceLabel := chi.URLParam(r, "instanceLabel")
	if instanceLabel == "" {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "missing instance label in URL")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "missing token query parameter")
		return
	}

	claims, err := auth.VerifyConnectToken(token, h.jwtSecret)
	if err != nil {
		if errors.Is(err, auth.ErrTokenExpired) {
			slog.Info("WSHandler.ServeHTTP: connect token expired — client should re-resolve",
				"instanceLabel", instanceLabel)
			writeError(w, http.StatusUnauthorized, game.ErrCodeConnectTokenExpired,
				"connect token expired — call GET /games/:id/resolve again and dial the fresh wsPath it returns; do not retry this URL")
			return
		}
		slog.Warn("WSHandler.ServeHTTP: token verification failed", "instanceLabel", instanceLabel, "error", err)
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "invalid token")
		return
	}

	if claims.InstanceLabel != instanceLabel {
		slog.Warn("WSHandler.ServeHTTP: token instanceLabel does not match URL",
			"urlInstanceLabel", instanceLabel, "tokenInstanceLabel", claims.InstanceLabel)
		writeError(w, http.StatusUnauthorized, game.ErrCodeInvalidToken, "token does not match this instance")
		return
	}

	gameID := claims.GameID
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
			h.manager.HandleDisconnect(h.ctx, gameID, color)
			h.wsRegistry.Unregister(connID)
		},
	)
}
