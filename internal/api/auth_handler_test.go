//go:build integration

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/store"
)

// newTestAuthServer wires AuthHandler behind a chi router exactly as
// routes.go's NewRouter does for /users and /login. Same pattern as
// newTestGameServer in game_handler_test.go — a narrowly-scoped router
// mounting only the handler under test.
func newTestAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	userStore := store.NewUserStore(testPool)
	handler := NewAuthHandler(userStore)

	r := chi.NewRouter()
	r.Post("/users", handler.Register)
	r.Post("/login", handler.Login)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// --- Register (POST /users) ---

func TestAuthHandler_Register_Success(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	username := "alice_" + uuid.NewString()[:8]
	resp := postJSON(t, srv, "/users", map[string]string{"username": username, "password": "correct-horse-battery"})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeData[authResponseData](t, resp)
	if data.UserID == "" {
		t.Error("expected a non-empty userID")
	}
}

func TestAuthHandler_Register_DuplicateUsername(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	username := "bob_" + uuid.NewString()[:8]
	first := postJSON(t, srv, "/users", map[string]string{"username": username, "password": "first-password"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first register: expected 201, got %d", first.StatusCode)
	}

	second := postJSON(t, srv, "/users", map[string]string{"username": username, "password": "second-password"})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second register: expected 409, got %d", second.StatusCode)
	}
	errDetail := decodeErr(t, second)
	if errDetail.Code != errCodeUsernameTaken {
		t.Errorf("expected code %q, got %q", errCodeUsernameTaken, errDetail.Code)
	}
}

func TestAuthHandler_Register_PasswordTooShort(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	resp := postJSON(t, srv, "/users", map[string]string{"username": "shortpw", "password": "short"})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", errCodeInvalidRequest, errDetail.Code)
	}
}

func TestAuthHandler_Register_UsernameTooShort(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	resp := postJSON(t, srv, "/users", map[string]string{"username": "ab", "password": "long-enough-password"})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", errCodeInvalidRequest, errDetail.Code)
	}
}

// --- Login (POST /login) ---

func TestAuthHandler_Login_Success(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	username := "carol_" + uuid.NewString()[:8]
	reg := postJSON(t, srv, "/users", map[string]string{"username": username, "password": "carols-real-password"})
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d", reg.StatusCode)
	}
	registered := decodeData[authResponseData](t, reg)

	resp := postJSON(t, srv, "/login", map[string]string{"username": username, "password": "carols-real-password"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[authResponseData](t, resp)

	if data.UserID != registered.UserID {
		t.Errorf("login returned a different userID: got %q, want %q (the one Register returned)", data.UserID, registered.UserID)
	}
}

func TestAuthHandler_Login_WrongPassword(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	username := "dave_" + uuid.NewString()[:8]
	reg := postJSON(t, srv, "/users", map[string]string{"username": username, "password": "daves-real-password"})
	if reg.StatusCode != http.StatusCreated {
		t.Fatalf("register: expected 201, got %d", reg.StatusCode)
	}

	resp := postJSON(t, srv, "/login", map[string]string{"username": username, "password": "wrong-password"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidCredentials {
		t.Errorf("expected code %q, got %q", errCodeInvalidCredentials, errDetail.Code)
	}
}

func TestAuthHandler_Login_UnknownUsername(t *testing.T) {
	truncateAll(t)
	srv := newTestAuthServer(t)

	resp := postJSON(t, srv, "/login", map[string]string{"username": "nobody_" + uuid.NewString()[:8], "password": "irrelevant-password"})

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	// Same code and status as a wrong-password result above — this is the
	// actual behavior under test: AuthHandler.Login must not let a caller
	// distinguish "unknown username" from "wrong password" (see
	// internal/auth/password.go's doc comment on the enumeration side
	// channel this is closing).
	if errDetail.Code != errCodeInvalidCredentials {
		t.Errorf("expected code %q, got %q", errCodeInvalidCredentials, errDetail.Code)
	}
}
