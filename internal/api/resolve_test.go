//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

const resolveTestInstanceID = "instance-a"

// newTestManagerWithDirectory mirrors internal/game/resolve_test.go's helper
// of the same purpose — a Manager wired to a real RedisDirectory
// (testRedisClient, DB 1), separate from ws_handler_test.go's newTestManager
// (directory=nil), since most tests in this package don't need Redis at all.
func newTestManagerWithDirectory(t *testing.T) *game.Manager {
	t.Helper()
	registry := game.NewGameRegistry()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	eventBus := game.NewLocalEventBus()
	processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
	directory := game.NewRedisDirectory(testRedisClient)
	return game.NewManager(registry, processor, gameStore, moveStore, eventBus, testJWTSecret, validator, directory, resolveTestInstanceID)
}

// newTestGameServerWithResolve wires GameHandler with resolve support
// (jwtSecret) behind a chi router including GET /games/{id}/resolve.
// Separate from game_handler_test.go's newTestGameServer because that one's
// NewGameHandler call predates jwtSecret being a required constructor
// parameter for resolve support — kept minimal there rather than touching
// every existing non-resolve test's server wiring.
func newTestGameServerWithResolve(t *testing.T, manager *game.Manager) *httptest.Server {
	t.Helper()
	userStore := store.NewUserStore(testPool)
	handler := NewGameHandler(manager, userStore, testJWTSecret)

	r := chi.NewRouter()
	r.Post("/games", handler.CreateGame)
	r.Post("/games/{id}/join", handler.JoinGame)
	r.Get("/games/{id}", handler.GetGame)
	r.Get("/games/{id}/resolve", handler.Resolve)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// getWithBearer issues a GET request carrying an Authorization: Bearer
// header — game_handler_test.go's existing getJSON helper has no way to set
// custom headers, and Resolve deliberately uses a header instead of a query
// parameter (see GameHandler.Resolve's doc comment), so a new helper is
// needed rather than reusing getJSON.
func getWithBearer(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestGameHandler_Resolve_Success(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	manager := newTestManagerWithDirectory(t)
	srv := newTestGameServerWithResolve(t, manager)

	whiteID := uuid.NewString()
	mustCreateUser(t, whiteID)

	session, whiteToken, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	resp := getWithBearer(t, srv, "/games/"+session.ID+"/resolve", whiteToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[resolveResponseData](t, resp)

	if data.ConnectToken == "" {
		t.Fatal("expected non-empty connectToken")
	}
	if data.InstanceLabel != resolveTestInstanceID {
		t.Errorf("instanceLabel: got %q, want %q", data.InstanceLabel, resolveTestInstanceID)
	}
	if data.WSPath != "/connect/"+resolveTestInstanceID {
		t.Errorf("wsPath: got %q, want %q", data.WSPath, "/connect/"+resolveTestInstanceID)
	}

	claims, err := auth.VerifyConnectToken(data.ConnectToken, testJWTSecret)
	if err != nil {
		t.Fatalf("VerifyConnectToken: %v", err)
	}
	if claims.GameID != session.ID || claims.UserID != whiteID || claims.Color != string(store.ColorWhite) {
		t.Errorf("unexpected connect claims: %+v", claims)
	}
}

func TestGameHandler_Resolve_MissingAuthHeader(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	manager := newTestManagerWithDirectory(t)
	srv := newTestGameServerWithResolve(t, manager)

	resp := getWithBearer(t, srv, "/games/"+uuid.NewString()+"/resolve", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != game.ErrCodeInvalidToken {
		t.Errorf("expected code %q, got %q", game.ErrCodeInvalidToken, errDetail.Code)
	}
}

func TestGameHandler_Resolve_TokenGameIDMismatch(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	manager := newTestManagerWithDirectory(t)
	srv := newTestGameServerWithResolve(t, manager)

	whiteID := uuid.NewString()
	mustCreateUser(t, whiteID)

	_, whiteToken, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	// Token is valid but scoped to a DIFFERENT game than the one in the URL.
	otherGameID := uuid.NewString()
	resp := getWithBearer(t, srv, "/games/"+otherGameID+"/resolve", whiteToken)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestGameHandler_Resolve_NonexistentGame(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	manager := newTestManagerWithDirectory(t)
	srv := newTestGameServerWithResolve(t, manager)

	// A validly-signed PlayerClaims token for a gameID that was never
	// actually created (auth.VerifyPlayerToken has no way to know this —
	// only Manager.ResolveGame's downstream GetGame call does).
	fakeGameID := uuid.NewString()
	userID := uuid.NewString()
	claims := auth.PlayerClaims{
		GameID: fakeGameID,
		UserID: userID,
		Color:  string(store.ColorWhite),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token, err := auth.SignPlayerToken(claims, testJWTSecret)
	if err != nil {
		t.Fatalf("SignPlayerToken: %v", err)
	}

	resp := getWithBearer(t, srv, "/games/"+fakeGameID+"/resolve", token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != game.ErrCodeGameNotFound {
		t.Errorf("expected code %q, got %q", game.ErrCodeGameNotFound, errDetail.Code)
	}
}

// TestGameHandler_Resolve_TerminalGame_Succeeds is the HTTP-level companion
// to internal/game/resolve_test.go's TestManager_ResolveGame_TerminalGame_Succeeds
// — see that test for the detailed rationale (PHASE_2.md Step 5/11's
// explicit requirement that resolving an already-completed game succeeds).
// This test only confirms the HTTP contract (200, valid envelope); the core
// hydration/goroutine-leak behavior is covered at the Manager level.
func TestGameHandler_Resolve_TerminalGame_Succeeds(t *testing.T) {
	truncateAll(t)
	flushTestRedisDB(t)

	manager := newTestManagerWithDirectory(t)
	srv := newTestGameServerWithResolve(t, manager)

	whiteID := uuid.NewString()
	blackID := uuid.NewString()
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	session, _, err := manager.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	blackToken, err := manager.JoinGame(ctx, session.ID, blackID)
	if err != nil {
		t.Fatalf("JoinGame: %v", err)
	}
	// See internal/game/resolve_test.go's identical comment: both the
	// in-memory Transition and the DB-level UpdateGameStatus are required to
	// correctly simulate a real HandleConnect activation, or the later
	// resignation's UpdateGameStatus(fromStatus=ACTIVE) silently no-ops
	// against a DB row still stuck at WAITING_FOR_PLAYER.
	if err := session.Transition(store.GameStatusActive); err != nil {
		t.Fatalf("Transition to ACTIVE: %v", err)
	}
	if err := store.NewGameStore(testPool).UpdateGameStatus(ctx, session.ID, store.GameStatusWaiting, store.GameStatusActive, nil); err != nil {
		t.Fatalf("UpdateGameStatus to ACTIVE: %v", err)
	}

	// Resign directly against the manager (no live WS connections in this
	// HTTP-only test) — same effect as a real resignation: COMPLETED status,
	// session unregistered via finalizeGame.
	if err := manager.HandleMessage(ctx, session.ID, store.ColorWhite, []byte(`{"type":"RESIGN"}`)); err != nil {
		t.Fatalf("HandleMessage RESIGN: %v", err)
	}

	resp := getWithBearer(t, srv, "/games/"+session.ID+"/resolve", blackToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 resolving a terminal game, got %d", resp.StatusCode)
	}
	data := decodeData[resolveResponseData](t, resp)
	if data.ConnectToken == "" {
		t.Fatal("expected non-empty connectToken even for a terminal game")
	}
}
