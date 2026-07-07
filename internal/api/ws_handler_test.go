//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

const testJWTSecret = "test-secret-for-ws-handler-tests"

// newTestManager builds a fully-wired *game.Manager against the shared
// testPool — no mocks, per CODING_GUIDELINES.md §6 (store tests use real
// PostgreSQL). Mirrors production wiring exactly; if this diverges from how
// cmd/server/main.go (Step 13) actually constructs a Manager, that is itself
// a signal something is wrong with one of the two.
func newTestManager(t *testing.T) *game.Manager {
	t.Helper()
	registry := game.NewGameRegistry()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	eventBus := game.NewLocalEventBus()
	processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
	return game.NewManager(registry, processor, gameStore, moveStore, eventBus, testJWTSecret, validator)
}

// newTestServer wires WSHandler behind a chi router exactly as Step 12's
// internal/api/routes.go will, and returns an httptest.Server.
func newTestServer(t *testing.T, manager *game.Manager) *httptest.Server {
	t.Helper()
	wsRegistry := ws.NewRegistry()
	handler := NewWSHandler(context.Background(), manager, wsRegistry, testJWTSecret)

	r := chi.NewRouter()
	r.Get("/ws/game/{id}", handler.ServeHTTP)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// wsURL converts an httptest.Server's http:// base URL into a ws:// URL for
// the given game ID and token.
func wsURL(srv *httptest.Server, gameID, token string) string {
	base := strings.TrimPrefix(srv.URL, "http://")
	return "ws://" + base + "/ws/game/" + gameID + "?token=" + token
}

func statusOrZero(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// dial opens a WebSocket connection and fails the test immediately if the
// dial itself fails (as opposed to failing later on a specific assertion).
func dial(t *testing.T, srv *httptest.Server, gameID, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, gameID, token), nil)
	if err != nil {
		t.Fatalf("dial gameID=%s: %v (status=%d)", gameID, err, statusOrZero(resp))
	}
	return conn
}

// readOne reads a single message with a bounded deadline, so a broken
// implementation fails the test with a clear timeout instead of hanging the
// test binary. This is a deadline on a blocking read, not a sleep used as
// synchronization — CODING_GUIDELINES.md §8 forbids the latter, not the
// former.
func readOne(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return raw
}

func assertMessageType(t *testing.T, conn *websocket.Conn, expected string) {
	t.Helper()
	raw := readOne(t, conn)
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal message: %v (raw=%s)", err, raw)
	}
	if m.Type != expected {
		t.Errorf("expected message type %q, got %q (raw=%s)", expected, m.Type, raw)
	}
}

// TestWSHandler_InvalidToken_RefusedBeforeUpgrade covers PHASE_1.md Step 11's
// first required case: an invalid token must be refused with HTTP 401 before
// any WebSocket upgrade is attempted.
func TestWSHandler_InvalidToken_RefusedBeforeUpgrade(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, "some-game-id", "not-a-real-token"), nil)
	if err == nil {
		t.Fatal("expected dial to fail for an invalid token, but it succeeded")
	}
	if resp == nil {
		t.Fatal("expected an HTTP response accompanying the dial failure")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected HTTP 401, got %d", resp.StatusCode)
	}
}

// TestWSHandler_ValidToken_ReceivesGameState covers the second required case:
// a valid token is accepted and the connecting player receives GAME_STATE.
// Only White has connected — the game is still WAITING_FOR_PLAYER.
func TestWSHandler_ValidToken_ReceivesGameState(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	mustCreateUser(t, whiteID)

	session, whiteToken, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	conn := dial(t, srv, session.ID, whiteToken)
	defer conn.Close()

	raw := readOne(t, conn)
	var msg struct {
		Type   string `json:"type"`
		Turn   string `json:"turn"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal GAME_STATE: %v (raw=%s)", err, raw)
	}

	if msg.Type != "GAME_STATE" {
		t.Errorf("expected GAME_STATE, got %q", msg.Type)
	}
	if msg.Turn != "WHITE" {
		t.Errorf("expected turn WHITE, got %q", msg.Turn)
	}
	if msg.Status != "WAITING_FOR_PLAYER" {
		t.Errorf("expected status WAITING_FOR_PLAYER (Black has not joined yet), got %q", msg.Status)
	}
}

// TestWSHandler_Reconnect_ReceivesCurrentGameState covers the third required
// case: a second connection with the same token receives the current
// GAME_STATE, and the original connection's session state is reflected
// correctly.
//
// This opens the second White connection while the first is still live,
// rather than closing the first and waiting for the server to notice the
// drop. Both exercise the identical code path inside GameSession —
// RegisterConnection returning ErrConnectionOccupied, followed by
// ReplaceConnection — but the concurrent-connection version is deterministic:
// waiting on an async TCP-close detection would require either a fixed sleep
// or a polling loop, both of which CODING_GUIDELINES.md §8 flags as sources
// of test flakiness. Blocking, deadline-bounded ReadMessage calls are used
// throughout instead, which is real synchronization, not a guess at timing.
func TestWSHandler_Reconnect_ReceivesCurrentGameState(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	blackID := uuid.NewString()
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	session, whiteToken, err := manager.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	blackToken, err := manager.JoinGame(ctx, session.ID, blackID)
	if err != nil {
		t.Fatalf("JoinGame: %v", err)
	}

	whiteConn1 := dial(t, srv, session.ID, whiteToken)
	defer whiteConn1.Close()
	assertMessageType(t, whiteConn1, "GAME_STATE") // WAITING_FOR_PLAYER — Black hasn't joined yet

	blackConn := dial(t, srv, session.ID, blackToken)
	defer blackConn.Close()
	assertMessageType(t, blackConn, "GAME_STATE") // Black's connect activates the game

	// Black's connect activated the game — White's original connection must
	// now receive OPPONENT_CONNECTED.
	assertMessageType(t, whiteConn1, "OPPONENT_CONNECTED")

	// Second connection using White's same token, while the first connection
	// is still open.
	whiteConn2 := dial(t, srv, session.ID, whiteToken)
	defer whiteConn2.Close()

	raw := readOne(t, whiteConn2)
	var msg struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal reconnect GAME_STATE: %v (raw=%s)", err, raw)
	}
	if msg.Type != "GAME_STATE" {
		t.Fatalf("expected GAME_STATE on reconnect, got %q", msg.Type)
	}
	if msg.Status != "ACTIVE" {
		t.Errorf("expected status ACTIVE (both players connected), got %q", msg.Status)
	}

	// Black should also observe White's second connection as a reconnection.
	assertMessageType(t, blackConn, "OPPONENT_RECONNECTED")
}
