package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/vedant-2701/chess/internal/auth"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

// AuthHandler implements the simple username+password registration/login
// endpoints (PHASE_3.md addendum, fixes TD-P3-007). See
// internal/auth/password.go's doc comment for the full scope rationale —
// this handler returns a bare userID on success, with no token, no
// session, no expiration, by explicit instruction.
type AuthHandler struct {
	userStore *store.UserStore
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(userStore *store.UserStore) *AuthHandler {
	return &AuthHandler{userStore: userStore}
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResponseData struct {
	UserID string `json:"userID"`
}

const (
	minUsernameLen = 3
	maxUsernameLen = 32
	minPasswordLen = 8
)

// decodeCredentials decodes and validates a {"username","password"} body,
// shared by Register and Login. The length bounds are deliberately modest
// sanity checks, not a full password-strength policy — matching this
// addendum's "keep it simple" scope; a stronger policy is easy to layer on
// later without changing the endpoint shape.
func decodeCredentials(w http.ResponseWriter, r *http.Request, dst *credentialsRequest) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "malformed request body")
		return false
	}
	if len(dst.Username) < minUsernameLen || len(dst.Username) > maxUsernameLen {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest,
			fmt.Sprintf("username must be %d-%d characters", minUsernameLen, maxUsernameLen))
		return false
	}
	if len(dst.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest,
			fmt.Sprintf("password must be at least %d characters", minPasswordLen))
		return false
	}
	return true
}

// Register implements POST /users. Creates a new user identified by
// username+password and returns its generated userID — the client then
// uses that userID exactly as an anonymously-generated one, in every
// existing endpoint (POST /games, POST /matchmaking/queue, etc.).
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		slog.Error("AuthHandler.Register: HashPassword failed", "username", req.Username, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to register")
		return
	}

	user, err := h.userStore.CreateUserWithCredentials(r.Context(), req.Username, hash)
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			writeError(w, http.StatusConflict, errCodeUsernameTaken, "username already taken")
			return
		}
		slog.Error("AuthHandler.Register: CreateUserWithCredentials failed", "username", req.Username, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to register")
		return
	}

	writeData(w, http.StatusCreated, authResponseData{UserID: user.ID})
}

// Login implements POST /login. Verifies username+password and returns the
// matching userID — recovering the same stable ID Register originally
// handed out, e.g. from a new device or after clearing local storage.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}

	ctx := r.Context()
	userID, hash, err := h.userStore.GetUserIDAndPasswordHashByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			// Identical response to a wrong-password result below —
			// deliberately not distinguishing "no such username" from
			// "wrong password," to avoid a username-enumeration side
			// channel (see auth.VerifyPassword's doc comment).
			writeError(w, http.StatusUnauthorized, errCodeInvalidCredentials, "invalid username or password")
			return
		}
		slog.Error("AuthHandler.Login: GetUserIDAndPasswordHashByUsername failed", "username", req.Username, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to log in")
		return
	}

	ok, err := auth.VerifyPassword(req.Password, hash)
	if err != nil {
		slog.Error("AuthHandler.Login: VerifyPassword failed", "username", req.Username, "error", err)
		writeError(w, http.StatusInternalServerError, game.ErrCodeInternalError, "failed to log in")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeInvalidCredentials, "invalid username or password")
		return
	}

	writeData(w, http.StatusOK, authResponseData{UserID: userID})
}
