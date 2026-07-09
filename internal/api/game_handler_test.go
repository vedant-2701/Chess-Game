//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/auth"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

// newTestGameServer wires GameHandler behind a chi router exactly as
// Step 12's internal/api/routes.go does for the REST endpoints. The WS
// endpoint is deliberately not mounted here — that surface is already
// covered by ws_handler_test.go; this file exists specifically to close
// the gap flagged in CLAUDE.md's Known Sharp Edges: GameHandler's own
// HTTP-level behavior (status codes, envelope shape, JoinGame's four-way
// error branching) was previously only exercised implicitly, through
// newTestServer's wiring calling Manager.CreateGame/JoinGame directly, not
// through these handlers.
func newTestGameServer(t *testing.T, manager *game.Manager) *httptest.Server {
	t.Helper()
	userStore := store.NewUserStore(testPool)
	handler := NewGameHandler(manager, userStore)

	r := chi.NewRouter()
	r.Post("/games", handler.CreateGame)
	r.Post("/games/{id}/join", handler.JoinGame)
	r.Get("/games/{id}", handler.GetGame)
	r.Get("/health", handler.Health)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// postJSON POSTs a JSON-marshaled body and fails the test immediately on a
// transport-level failure (as opposed to failing later on a status/body
// assertion).
func postJSON(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// postRaw POSTs a raw byte body — used for the malformed-JSON test case,
// where postJSON's json.Marshal step would defeat the point.
func postRaw(t *testing.T, srv *httptest.Server, path string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeData decodes a {"data": ...} envelope (response.go's dataEnvelope
// shape) into the given type. Reusing the real unexported response types
// from game_handler.go/response.go rather than re-declaring parallel structs
// means these tests break if the actual encoded shape changes, not just if
// some independently-maintained copy of it changes.
func decodeData[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode data envelope: %v", err)
	}
	return env.Data
}

// decodeErr decodes a {"error": {"code", "message"}} envelope.
func decodeErr(t *testing.T, resp *http.Response) errorDetail {
	t.Helper()
	var env struct {
		Error errorDetail `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error
}

// createGameHTTP drives game creation entirely through the HTTP layer
// (rather than calling manager.CreateGame directly) so setup for later
// steps (JoinGame, GetGame) exercises the same handler code under test,
// not a shortcut around it.
func createGameHTTP(t *testing.T, srv *httptest.Server) (gameID, whiteUserID, whiteToken string) {
	t.Helper()
	whiteUserID = uuid.NewString()
	resp := postJSON(t, srv, "/games", map[string]string{"userID": whiteUserID})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("createGameHTTP: expected 201, got %d", resp.StatusCode)
	}
	data := decodeData[createGameResponseData](t, resp)
	return data.GameID, whiteUserID, data.PlayerToken
}

// --- CreateGame (POST /games) ---

func TestGameHandler_CreateGame_Success(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	userID := uuid.NewString()
	resp := postJSON(t, srv, "/games", map[string]string{"userID": userID})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeData[createGameResponseData](t, resp)

	if data.GameID == "" {
		t.Error("expected non-empty gameID")
	}
	if data.Color != string(store.ColorWhite) {
		t.Errorf("expected color WHITE, got %q", data.Color)
	}
	if data.JoinURL != "/game/"+data.GameID {
		t.Errorf("expected joinURL /game/%s, got %q", data.GameID, data.JoinURL)
	}
	if data.PlayerToken == "" {
		t.Fatal("expected non-empty playerToken")
	}

	// The token is the actual contract with the client — verify it decodes
	// to claims matching what was just created, not just that some string
	// was returned.
	claims, err := auth.VerifyPlayerToken(data.PlayerToken, testJWTSecret)
	if err != nil {
		t.Fatalf("VerifyPlayerToken: %v", err)
	}
	if claims.GameID != data.GameID {
		t.Errorf("token gameID = %q, want %q", claims.GameID, data.GameID)
	}
	if claims.UserID != userID {
		t.Errorf("token userID = %q, want %q", claims.UserID, userID)
	}
	if claims.Color != string(store.ColorWhite) {
		t.Errorf("token color = %q, want WHITE", claims.Color)
	}
}

func TestGameHandler_CreateGame_InvalidUserID(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	resp := postJSON(t, srv, "/games", map[string]string{"userID": "not-a-uuid"})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", errCodeInvalidRequest, errDetail.Code)
	}
}

func TestGameHandler_CreateGame_MalformedBody(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	resp := postRaw(t, srv, "/games", []byte(`{"userID": `)) // truncated JSON

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", errCodeInvalidRequest, errDetail.Code)
	}
}

// --- JoinGame (POST /games/{id}/join) ---

func TestGameHandler_JoinGame_Success(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	gameID, _, _ := createGameHTTP(t, srv)
	blackUserID := uuid.NewString()

	resp := postJSON(t, srv, "/games/"+gameID+"/join", map[string]string{"userID": blackUserID})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[joinGameResponseData](t, resp)

	if data.GameID != gameID {
		t.Errorf("expected gameID %q, got %q", gameID, data.GameID)
	}
	if data.Color != string(store.ColorBlack) {
		t.Errorf("expected color BLACK, got %q", data.Color)
	}

	claims, err := auth.VerifyPlayerToken(data.PlayerToken, testJWTSecret)
	if err != nil {
		t.Fatalf("VerifyPlayerToken: %v", err)
	}
	if claims.GameID != gameID || claims.UserID != blackUserID || claims.Color != string(store.ColorBlack) {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestGameHandler_JoinGame_GameNotFound(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	resp := postJSON(t, srv, "/games/"+uuid.NewString()+"/join", map[string]string{"userID": uuid.NewString()})

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != game.ErrCodeGameNotFound {
		t.Errorf("expected code %q, got %q", game.ErrCodeGameNotFound, errDetail.Code)
	}
}

func TestGameHandler_JoinGame_AlreadyJoined(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	gameID, _, _ := createGameHTTP(t, srv)

	first := postJSON(t, srv, "/games/"+gameID+"/join", map[string]string{"userID": uuid.NewString()})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first join: expected 200, got %d", first.StatusCode)
	}

	second := postJSON(t, srv, "/games/"+gameID+"/join", map[string]string{"userID": uuid.NewString()})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second join: expected 409, got %d", second.StatusCode)
	}
	errDetail := decodeErr(t, second)
	if errDetail.Code != errCodeGameNotJoinable {
		t.Errorf("expected code %q, got %q", errCodeGameNotJoinable, errDetail.Code)
	}
}

func TestGameHandler_JoinGame_SelfPlay(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	gameID, whiteUserID, _ := createGameHTTP(t, srv)

	resp := postJSON(t, srv, "/games/"+gameID+"/join", map[string]string{"userID": whiteUserID})

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeSelfPlayDisallow {
		t.Errorf("expected code %q, got %q", errCodeSelfPlayDisallow, errDetail.Code)
	}
}

func TestGameHandler_JoinGame_InvalidUserID(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	gameID, _, _ := createGameHTTP(t, srv)

	resp := postJSON(t, srv, "/games/"+gameID+"/join", map[string]string{"userID": "not-a-uuid"})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("expected code %q, got %q", errCodeInvalidRequest, errDetail.Code)
	}
}

// --- GetGame (GET /games/{id}) ---

func TestGameHandler_GetGame_Success(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	gameID, _, _ := createGameHTTP(t, srv)

	resp := getJSON(t, srv, "/games/"+gameID)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[getGameResponseData](t, resp)

	if data.GameID != gameID {
		t.Errorf("expected gameID %q, got %q", gameID, data.GameID)
	}
	if data.Status != string(store.GameStatusWaiting) {
		t.Errorf("expected status WAITING_FOR_PLAYER, got %q", data.Status)
	}
	if data.CurrentFEN != store.StartingFEN {
		t.Errorf("expected starting FEN, got %q", data.CurrentFEN)
	}
	if data.Outcome != nil {
		t.Errorf("expected nil outcome, got %q", *data.Outcome)
	}
	if data.OutcomeReason != nil {
		t.Errorf("expected nil outcomeReason, got %q", *data.OutcomeReason)
	}
}

func TestGameHandler_GetGame_NotFound(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	resp := getJSON(t, srv, "/games/"+uuid.NewString())

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	errDetail := decodeErr(t, resp)
	if errDetail.Code != game.ErrCodeGameNotFound {
		t.Errorf("expected code %q, got %q", game.ErrCodeGameNotFound, errDetail.Code)
	}
}

// --- Health (GET /health) ---

// TestGameHandler_Health_FlatEnvelope asserts not just 200/"ok", but that
// the body is genuinely flat — no top-level "data" key — since that's the
// entire point of the documented CODING_GUIDELINES.md §7 exception (see
// CLAUDE.md Implementation Decisions). A test that only checked
// Status == "ok" would pass even if someone "fixed" Health to route through
// writeData and broke the documented exception silently.
func TestGameHandler_Health_FlatEnvelope(t *testing.T) {
	truncateAll(t)
	srv := newTestGameServer(t, newTestManager(t))

	resp := getJSON(t, srv, "/health")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode health body: %v", err)
	}

	if _, hasData := raw["data"]; hasData {
		t.Error("expected no top-level \"data\" key in /health response — it must be flat, not enveloped")
	}
	if _, hasError := raw["error"]; hasError {
		t.Error("unexpected top-level \"error\" key in /health response")
	}

	statusRaw, ok := raw["status"]
	if !ok {
		t.Fatal("expected top-level \"status\" key in /health response")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status != "ok" {
		t.Errorf("expected status \"ok\", got %q", status)
	}
}
