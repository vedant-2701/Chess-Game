package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

// GameHandler implements the REST endpoints for game creation, joining, and
// status lookup (PHASE_1.md Step 12).
//
// PHASE_1.md's Step 12 spec lists GameHandler as depending only on
// game.Manager. In practice it also needs *store.UserStore directly:
// Manager.CreateGame/JoinGame both assume the calling user record already
// exists (see manager.go's own comments on those methods) — user upsert is
// an HTTP-layer concern (turning a client-supplied anonymous UUID into a
// real row), not something Manager should own. Adding UserStore here is a
// one-line deviation from the literal spec, not a new architectural
// decision — internal/store has no dependency on internal/game or
// internal/ws, so this does not risk any import cycle (see ARCHITECTURE.md's
// Dependency Graph).
type GameHandler struct {
	manager   *game.Manager
	userStore *store.UserStore
}

// NewGameHandler constructs a GameHandler.
func NewGameHandler(manager *game.Manager, userStore *store.UserStore) *GameHandler {
	return &GameHandler{manager: manager, userStore: userStore}
}

type createGameResponseData struct {
	GameID      string `json:"gameID"`
	PlayerToken string `json:"playerToken"`
	Color       string `json:"color"`
	JoinURL     string `json:"joinURL"`
}

type joinGameResponseData struct {
	GameID      string `json:"gameID"`
	PlayerToken string `json:"playerToken"`
	Color       string `json:"color"`
}

type getGameResponseData struct {
	GameID        string  `json:"gameID"`
	Status        string  `json:"status"`
	CurrentFEN    string  `json:"currentFEN"`
	Outcome       *string `json:"outcome"`
	OutcomeReason *string `json:"outcomeReason"`
}

type healthResponseData struct {
	Status string `json:"status"`
}

// decodeAndValidateUserID decodes a {"userID": "..."} body and validates it
// is a well-formed UUID. Returns false (having already written the error
// response) if either step fails, so callers can just `if !ok { return }`.
//
// Validating here rather than letting a malformed value reach Postgres
// matters concretely: users.id is a UUID column (ARCHITECTURE.md schema),
// so a non-UUID string would otherwise surface as an opaque DB type error
// instead of a clear 400.
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

// CreateGame implements POST /games.
func (h *GameHandler) CreateGame(w http.ResponseWriter, r *http.Request) {
	var userID string
	if !decodeAndValidateUserID(w, r, &userID) {
		return
	}

	ctx := r.Context()
	if _, err := h.userStore.CreateOrGetUser(ctx, userID); err != nil {
		slog.Error("GameHandler.CreateGame: CreateOrGetUser failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to create user")
		return
	}

	session, token, err := h.manager.CreateGame(ctx, userID)
	if err != nil {
		slog.Error("GameHandler.CreateGame: Manager.CreateGame failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to create game")
		return
	}

	writeData(w, http.StatusCreated, createGameResponseData{
		GameID:      session.ID,
		PlayerToken: token,
		Color:       string(store.ColorWhite),
		JoinURL:     "/game/" + session.ID,
	})
}

// JoinGame implements POST /games/{id}/join.
func (h *GameHandler) JoinGame(w http.ResponseWriter, r *http.Request) {
	gameID := chi.URLParam(r, "id")
	if gameID == "" {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "missing game id in URL")
		return
	}

	var userID string
	if !decodeAndValidateUserID(w, r, &userID) {
		return
	}

	ctx := r.Context()
	if _, err := h.userStore.CreateOrGetUser(ctx, userID); err != nil {
		slog.Error("GameHandler.JoinGame: CreateOrGetUser failed", "userID", userID, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to create user")
		return
	}

	token, err := h.manager.JoinGame(ctx, gameID, userID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrGameNotFound):
			// The DB row genuinely does not exist — a legitimate 404.
			writeError(w, http.StatusNotFound, game.ErrCodeGameNotFound, "game not found")

		case errors.Is(err, game.ErrGameNotJoinable):
			writeError(w, http.StatusConflict, errCodeGameNotJoinable,
				"game already has two players or is no longer joinable")

		case errors.Is(err, game.ErrSelfPlay):
			writeError(w, http.StatusConflict, errCodeSelfPlayDisallow, "cannot join your own game")

		case errors.Is(err, game.ErrGameNotFound):
			// The DB row exists but there is no in-memory GameSession for it.
			// This should only be reachable if a WAITING_FOR_PLAYER game was
			// never restored after a server restart — a server-side
			// consistency bug, not a client-facing "not found". Deliberately
			// not conflated with the store.ErrGameNotFound case above: from
			// the client's perspective the game exists (it's on their
			// screen), so 404 would be actively misleading.
			slog.Error("GameHandler.JoinGame: game exists in DB but has no in-memory session",
				"gameID", gameID, "userID", userID, "error", err)
			writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "game session unavailable")

		default:
			slog.Error("GameHandler.JoinGame: Manager.JoinGame failed",
				"gameID", gameID, "userID", userID, "error", err)
			writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to join game")
		}
		return
	}

	writeData(w, http.StatusOK, joinGameResponseData{
		GameID:      gameID,
		PlayerToken: token,
		Color:       string(store.ColorBlack),
	})
}

// GetGame implements GET /games/{id}.
func (h *GameHandler) GetGame(w http.ResponseWriter, r *http.Request) {
	gameID := chi.URLParam(r, "id")
	if gameID == "" {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "missing game id in URL")
		return
	}

	g, err := h.manager.GetGame(r.Context(), gameID)
	if err != nil {
		if errors.Is(err, store.ErrGameNotFound) {
			writeError(w, http.StatusNotFound, game.ErrCodeGameNotFound, "game not found")
			return
		}
		slog.Error("GameHandler.GetGame: failed", "gameID", gameID, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to fetch game")
		return
	}

	var outcome, outcomeReason *string
	if g.Outcome != nil {
		s := string(*g.Outcome)
		outcome = &s
	}
	if g.OutcomeReason != nil {
		s := string(*g.OutcomeReason)
		outcomeReason = &s
	}

	writeData(w, http.StatusOK, getGameResponseData{
		GameID:        g.ID,
		Status:        string(g.Status),
		CurrentFEN:    g.CurrentFEN,
		Outcome:       outcome,
		OutcomeReason: outcomeReason,
	})
}

// Health implements GET /health. No dependencies — a true liveness check
// must not depend on the database being reachable (that's what /ready would
// be for, if this project distinguished the two; PHASE_1.md specifies only
// a single liveness endpoint).
//
// Deliberately NOT wrapped in the {"data": ...} envelope CODING_GUIDELINES.md
// §7 otherwise requires everywhere in this file. PHASE_1.md's own spec shows
// this endpoint's response as the flat {"status": "ok"}, and that's also
// the more practically correct shape here: load balancer / orchestrator
// health probes conventionally expect the simplest possible flat body (or
// just check the status code), not an API response envelope meant for
// application clients. Treating this as a narrow, explicit exception to
// §7 rather than silently picking one of the two conflicting specs.
func (h *GameHandler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(healthResponseData{Status: "ok"}); err != nil {
		slog.Error("GameHandler.Health: failed to encode response", "error", err)
	}
}
