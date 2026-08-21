package mmsvc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/auth"
)

// Handler implements matchmaking-service's HTTP endpoints (PHASE_3.md's
// "New Endpoints" section). Mirrors internal/api.GameHandler's shape: a
// thin struct holding its dependencies, one method per endpoint,
// CODING_GUIDELINES.md §7's envelope on every response.
//
// Queue, Stream, Status, Cancel, and Health are all implemented, and both
// producers Stream/Status depend on — reportserver.go's ReportServer and
// sweep.go's Sweep — now exist and call Hub.Notify/Queue.SetResult.
type Handler struct {
	queue                *Queue
	hub                  *Hub
	jwtSecret            string
	matchmakingClaimsTTL time.Duration
}

// NewHandler constructs a Handler.
//
// matchmakingClaimsTTL is PHASE_3.md Step 5's addition — the caller
// (cmd/matchmaking-service/main.go) resolves MATCHMAKING_CLAIMS_TTL_SECONDS
// against auth.DefaultMatchmakingClaimsTTL once at startup and passes the
// result here; Handler.Queue uses this field directly rather than reading
// any package-level constant, same reasoning as internal/game.Manager's
// connectClaimsTTL field (CODING_GUIDELINES.md §5, no package-level
// mutable state).
func NewHandler(queue *Queue, hub *Hub, jwtSecret string, matchmakingClaimsTTL time.Duration) *Handler {
	return &Handler{queue: queue, hub: hub, jwtSecret: jwtSecret, matchmakingClaimsTTL: matchmakingClaimsTTL}
}

type queueResponseData struct {
	MatchmakingToken string `json:"matchmakingToken"`
}

type healthResponseData struct {
	Status string `json:"status"`
}

// decodeAndValidateUserID mirrors internal/api's identical helper exactly
// (same UUID-validation rationale — users.id is a UUID column chess-server
// owns) — duplicated, not imported, since internal/api is chess-server's
// package and this package must not depend on it.
func decodeAndValidateUserID(w http.ResponseWriter, r *http.Request, dst *string) bool {
	var body struct {
		UserID string `json:"userID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "malformed request body")
		return false
	}
	if _, err := uuid.Parse(body.UserID); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "userID must be a valid UUID")
		return false
	}
	*dst = body.UserID
	return true
}

// Queue implements POST /matchmaking/queue (PHASE_3.md's New Endpoints,
// Challenge 4's check-before-enqueue).
//
// Order of operations matters: the ADR-037 active-game check happens
// BEFORE any write, so a player who already has an active game is rejected
// with 409 and never touches the queue at all — not enqueued-then-rejected,
// which would leave a spurious queue entry to clean up. The "already
// queued" fact (PHASE_3.md Challenge 4's second check) is handled by
// Enqueue's own ZADD NX rather than a separate read here — see
// Queue.Enqueue's doc comment for why that read would be redundant.
func (h *Handler) Queue(w http.ResponseWriter, r *http.Request) {
	var userID string
	if !decodeAndValidateUserID(w, r, &userID) {
		return
	}

	ctx := r.Context()

	marker, hasActiveGame, err := h.queue.ActiveGame(ctx, userID)
	if err != nil {
		slog.Error("Handler.Queue: ActiveGame check failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "failed to check active game")
		return
	}
	if hasActiveGame {
		writeErrorWithExistingGame(w, http.StatusConflict, errCodeAlreadyInActiveGame,
			"player already has an active game", existingGame{
				GameID:        marker.GameID,
				PlayerToken:   marker.PlayerToken,
				InstanceLabel: marker.InstanceLabel,
				WSPath:        marker.WSPath,
			})
		return
	}

	if _, err := h.queue.Enqueue(ctx, userID); err != nil {
		slog.Error("Handler.Queue: Enqueue failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "failed to enqueue")
		return
	}

	claims := auth.MatchmakingClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(h.matchmakingClaimsTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token, err := auth.SignMatchmakingToken(claims, h.jwtSecret)
	if err != nil {
		slog.Error("Handler.Queue: SignMatchmakingToken failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "failed to mint matchmaking token")
		return
	}

	writeData(w, http.StatusOK, queueResponseData{MatchmakingToken: token})
}

// Health implements GET /health. Deliberately NOT wrapped in the
// {"data": ...} envelope — same narrow, explicit exception
// internal/api.GameHandler.Health documents (load balancer / orchestrator
// health probes expect a flat body, not an application-client envelope).
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(healthResponseData{Status: "ok"}); err != nil {
		slog.Error("Handler.Health: failed to encode response", "error", err)
	}
}

// verifyMatchmakingTokenFromQuery extracts and verifies a MatchmakingClaims
// token passed as ?token=..., the convention every matchmaking-service
// endpoint after POST /matchmaking/queue uses (Stream, Status, Cancel) — a
// query parameter, not an Authorization header, primarily because GET
// /matchmaking/stream must work with the browser EventSource API, which
// cannot set custom headers; Status and Cancel follow the same convention
// for consistency rather than introducing a second transmission scheme for
// the same token type (PHASE_3.md's own endpoint list is ambiguous on this
// point for DELETE — "Header/token" — resolved here in favor of the one
// scheme this package already needs regardless). Writes its own error
// response and returns ok=false on any failure — callers just return early.
func (h *Handler) verifyMatchmakingTokenFromQuery(w http.ResponseWriter, r *http.Request) (claims *auth.MatchmakingClaims, ok bool) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusUnauthorized, errCodeInvalidRequest, "missing token")
		return nil, false
	}
	claims, err := auth.VerifyMatchmakingToken(token, h.jwtSecret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errCodeInvalidRequest, "invalid or expired token")
		return nil, false
	}
	return claims, true
}

// Stream implements GET /matchmaking/stream?token=<matchmakingToken>
// (PHASE_3.md's New Endpoints, ADR-036 §15). Verifies MatchmakingClaims
// once at connection open (mirrors WS upgrade handling — internal/ws's
// connect flow verifies its own claims once, the same way, not per-message),
// then holds the connection, relaying whatever this player's Hub channel
// receives as SSE events until the client disconnects.
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.verifyMatchmakingTokenFromQuery(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Cannot happen with net/http's standard ResponseWriter, but
		// CODING_GUIDELINES.md §1 forbids the naked type-assertion panic this
		// would otherwise be — internal packages never panic on a condition
		// that is reachable, even if only in principle via an unusual
		// http.Handler wrapper.
		slog.Error("Handler.Stream: ResponseWriter does not support flushing", "userID", claims.UserID)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := h.hub.register(claims.UserID)
	defer h.hub.unregister(claims.UserID, ch)

	slog.Debug("matchmaking stream opened", "userID", claims.UserID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			slog.Debug("matchmaking stream closed", "userID", claims.UserID)
			return
		case evt := <-ch:
			if err := writeSSEEvent(w, evt.event, evt.data); err != nil {
				slog.Warn("Handler.Stream: write failed, closing", "userID", claims.UserID, "error", err)
				return
			}
			flusher.Flush()
		}
	}
}

// statusResponseData is the union of GET /matchmaking/status's three
// possible shapes (PHASE_3.md's New Endpoints). omitempty on every field
// but Status so "waiting" serializes as exactly {"status":"waiting"}, not
// with a spray of empty strings. Two token fields for a "matched" result,
// same reasoning as matchFoundData/matchmakingResult (PHASE_3_DESIGN_NOTES.md
// §18): ConnectToken is directly dialable but may have expired if this is
// polled late; PlayerToken is the always-valid fallback for /resolve.
type statusResponseData struct {
	Status        string `json:"status"`
	GameID        string `json:"gameID,omitempty"`
	ConnectToken  string `json:"connectToken,omitempty"`
	PlayerToken   string `json:"playerToken,omitempty"`
	InstanceLabel string `json:"instanceLabel,omitempty"`
	WSPath        string `json:"wsPath,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Status implements GET /matchmaking/status?token=<matchmakingToken>
// (PHASE_3.md's New Endpoints — the lost-RPC/lost-SSE-push backstop,
// ADR-033's Consequences). Reads the same outcome record ReportServer
// (reportserver.go) and Sweep (sweep.go) write via Queue.SetResult;
// "waiting" is simply the absence of that record, not a distinct tracked
// state — this endpoint has no independent notion of "still queued" versus
// "was never queued at all," matching PHASE_3.md's own three-state contract
// exactly.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.verifyMatchmakingTokenFromQuery(w, r)
	if !ok {
		return
	}

	result, found, err := h.queue.GetResult(r.Context(), claims.UserID)
	if err != nil {
		slog.Error("Handler.Status: GetResult failed", "userID", claims.UserID, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "failed to check status")
		return
	}
	if !found {
		writeData(w, http.StatusOK, statusResponseData{Status: "waiting"})
		return
	}

	writeData(w, http.StatusOK, statusResponseData{
		Status:        result.Status,
		GameID:        result.GameID,
		ConnectToken:  result.ConnectToken,
		PlayerToken:   result.PlayerToken,
		InstanceLabel: result.InstanceLabel,
		WSPath:        result.WSPath,
		Reason:        result.Reason,
	})
}

type cancelResponseData struct {
	Status string `json:"status"`
}

// Cancel implements DELETE /matchmaking/queue (PHASE_3.md's New Endpoints,
// ADR-038). Plain synchronous response — no SSE push needed, the client
// already knows the outcome of its own cancel request. Does not also clear
// any GetResult record — by the time a client can meaningfully cancel
// (before a match is reported), no result record exists yet; if one is
// somehow already present the client is seeing MATCH_FOUND/MATCHMAKING_FAILED
// on its next Status poll regardless, and this endpoint has nothing useful
// to add to that outcome.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.verifyMatchmakingTokenFromQuery(w, r)
	if !ok {
		return
	}

	if err := h.queue.Dequeue(r.Context(), claims.UserID); err != nil {
		slog.Error("Handler.Cancel: Dequeue failed", "userID", claims.UserID, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "failed to dequeue")
		return
	}

	writeData(w, http.StatusOK, cancelResponseData{Status: "dequeued"})
}

// writeSSEEvent writes one SSE frame (PHASE_3.md's Match Notification
// section: "event: TYPE\ndata: JSON\n\n"). Not wrapped in the {"data":
// ...} envelope — SSE event payloads are explicitly out of scope for that
// convention per this document's own "New Endpoints" section note.
func writeSSEEvent(w http.ResponseWriter, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("mmsvc.writeSSEEvent event=%s: marshal: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return fmt.Errorf("mmsvc.writeSSEEvent event=%s: write: %w", event, err)
	}
	return nil
}
