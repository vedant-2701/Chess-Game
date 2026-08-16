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
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

const testJWTSecret = "test-secret-for-ws-handler-tests"

// testInstanceLabel is the fixed instance label these tests dial. Real
// production instanceLabels come from Manager.ResolveGame (see
// internal/game/resolve_test.go, internal/api/resolve_test.go for that
// coverage) — these WSHandler tests exercise connection lifecycle only and
// deliberately mint ConnectClaims directly (mustConnectToken below) rather
// than going through the full resolve flow, so any fixed string works as
// long as it's consistent between the minted token and the URL dialed.
const testInstanceLabel = "test-instance"

// newTestManager builds a fully-wired *game.Manager against the shared
// testPool — no mocks, per CODING_GUIDELINES.md §6 (store tests use real
// PostgreSQL). Mirrors production wiring exactly; if this diverges from how
// cmd/server/main.go actually constructs a Manager, that is itself a signal
// something is wrong with one of the two. directory=nil: these tests never
// call ResolveGame/StartHeartbeat — see NewManager's doc comment.
func newTestManager(t *testing.T) *game.Manager {
	t.Helper()
	registry := game.NewGameRegistry()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	validator := internalchess.NewValidator()
	eventBus := game.NewLocalEventBus()
	processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
	return game.NewManager(registry, processor, gameStore, moveStore, eventBus, testJWTSecret, validator, nil, "", auth.DefaultConnectClaimsTTL)
}

// newTestServer wires WSHandler behind a chi router exactly as
// internal/api/routes.go does, and returns an httptest.Server.
func newTestServer(t *testing.T, manager *game.Manager) *httptest.Server {
	t.Helper()
	wsRegistry := ws.NewRegistry()
	handler := NewWSHandler(context.Background(), manager, wsRegistry, testJWTSecret)

	r := chi.NewRouter()
	r.Get("/connect/{instanceLabel}", handler.ServeHTTP)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// mustConnectToken mints a ConnectClaims token directly via
// auth.SignConnectToken, bypassing the full resolve flow — see
// testInstanceLabel's doc comment for why that's the right scope for these
// tests. Uses auth.DefaultConnectClaimsTTL (the real production default
// value — Manager.ResolveGame's actual injected field is what these tests
// deliberately bypass, per this function's own doc comment), not an
// arbitrary test duration, so these tests exercise a realistic expiry
// window.
func mustConnectToken(t *testing.T, gameID, userID string, color store.Color) string {
	t.Helper()
	claims := auth.ConnectClaims{
		GameID:        gameID,
		UserID:        userID,
		Color:         string(color),
		InstanceLabel: testInstanceLabel,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(auth.DefaultConnectClaimsTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token, err := auth.SignConnectToken(claims, testJWTSecret)
	if err != nil {
		t.Fatalf("SignConnectToken: %v", err)
	}
	return token
}

// wsURL converts an httptest.Server's http:// base URL into a ws:// URL for
// the masked /connect/{instanceLabel} endpoint.
func wsURL(srv *httptest.Server, instanceLabel, token string) string {
	base := strings.TrimPrefix(srv.URL, "http://")
	return "ws://" + base + "/connect/" + instanceLabel + "?token=" + token
}

func statusOrZero(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// dial opens a WebSocket connection and fails the test immediately if the
// dial itself fails (as opposed to failing later on a specific assertion).
func dial(t *testing.T, srv *httptest.Server, instanceLabel, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, instanceLabel, token), nil)
	if err != nil {
		t.Fatalf("dial instanceLabel=%s: %v (status=%d)", instanceLabel, err, statusOrZero(resp))
	}
	return conn
}

// dialGame is a convenience wrapper combining mustConnectToken + dial for
// the common case (the game and instance label this test server expects).
func dialGame(t *testing.T, srv *httptest.Server, gameID, userID string, color store.Color) *websocket.Conn {
	t.Helper()
	return dial(t, srv, testInstanceLabel, mustConnectToken(t, gameID, userID, color))
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
// first required case, updated for PHASE_2.md Step 8's ConnectClaims-based
// connect path: an invalid token must be refused with HTTP 401 before any
// WebSocket upgrade is attempted.
func TestWSHandler_InvalidToken_RefusedBeforeUpgrade(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, testInstanceLabel, "not-a-real-token"), nil)
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

// TestWSHandler_ExpiredConnectToken_RejectsCleanly is PHASE_2.md Step 5's
// originally-deferred test, finally implementable now that WSHandler
// verifies ConnectClaims at all: a connect token that expired in the gap
// between resolve returning and the client actually dialing must be
// rejected cleanly (not panic, not hang) with the dedicated
// ErrCodeConnectTokenExpired code, distinct from a generically invalid
// token — the response body itself tells the client what to do next
// (re-call resolve, not retry this URL), per PHASE_2.md's exact wording for
// this test.
func TestWSHandler_ExpiredConnectToken_RejectsCleanly(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	mustCreateUser(t, whiteID)
	session, _, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	// Construct an already-expired ConnectClaims token directly — simulates
	// the client taking longer than the real ConnectClaimsTTL between resolve
	// returning and actually dialing.
	claims := auth.ConnectClaims{
		GameID:        session.ID,
		UserID:        whiteID,
		Color:         string(store.ColorWhite),
		InstanceLabel: testInstanceLabel,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-auth.DefaultConnectClaimsTTL - time.Second)),
		},
	}
	expiredToken, err := auth.SignConnectToken(claims, testJWTSecret)
	if err != nil {
		t.Fatalf("SignConnectToken: %v", err)
	}

	_, resp, dialErr := websocket.DefaultDialer.Dial(wsURL(srv, testInstanceLabel, expiredToken), nil)
	if dialErr == nil {
		t.Fatal("expected dial to fail for an expired connect token, but it succeeded")
	}
	if resp == nil {
		t.Fatal("expected an HTTP response accompanying the dial failure")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected HTTP 401, got %d", resp.StatusCode)
	}

	body := decodeErr(t, resp)
	if body.Code != game.ErrCodeConnectTokenExpired {
		t.Errorf("expected code %q, got %q", game.ErrCodeConnectTokenExpired, body.Code)
	}
	if body.Message == "" {
		t.Error("expected a non-empty message telling the client to re-resolve")
	}
}

// TestWSHandler_InstanceLabelMismatch_RejectsCleanly is the new integrity
// check PHASE_2.md Step 8 introduces (mirroring the old PlayerClaims path's
// "token gameID does not match URL" check): a token minted for a DIFFERENT
// instance than the one it's presented to must be rejected, not silently
// accepted just because the signature and gameID/color are otherwise valid.
func TestWSHandler_InstanceLabelMismatch_RejectsCleanly(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	mustCreateUser(t, whiteID)
	session, _, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	// Token is validly signed and otherwise correct, but names a DIFFERENT
	// instance label than the one it's dialed against below.
	token := mustConnectToken(t, session.ID, whiteID, store.ColorWhite)

	_, resp, dialErr := websocket.DefaultDialer.Dial(wsURL(srv, "some-other-instance", token), nil)
	if dialErr == nil {
		t.Fatal("expected dial to fail for a mismatched instance label, but it succeeded")
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

	session, _, err := manager.CreateGame(context.Background(), whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}

	conn := dialGame(t, srv, session.ID, whiteID, store.ColorWhite)
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
// case: a second connection with the same identity receives the current
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
	session, _, err := manager.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := manager.JoinGame(ctx, session.ID, blackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}

	whiteConn1 := dialGame(t, srv, session.ID, whiteID, store.ColorWhite)
	defer whiteConn1.Close()
	assertMessageType(t, whiteConn1, "GAME_STATE") // WAITING_FOR_PLAYER — Black hasn't joined yet

	blackConn := dialGame(t, srv, session.ID, blackID, store.ColorBlack)
	defer blackConn.Close()
	assertMessageType(t, blackConn, "GAME_STATE") // Black's connect activates the game

	// Black's connect activated the game — White's original connection must
	// now receive OPPONENT_CONNECTED.
	assertMessageType(t, whiteConn1, "OPPONENT_CONNECTED")

	// Second connection using White's identity, while the first connection
	// is still open.
	whiteConn2 := dialGame(t, srv, session.ID, whiteID, store.ColorWhite)
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

// assertConnectionClosedNormally reads the next frame and requires it to be
// a normal-closure (RFC 6455 code 1000) close frame, not another data
// message. Used to prove a connection was actually closed by the server,
// not merely that reading it eventually times out.
func assertConnectionClosedNormally(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected connection to be closed after GAME_OVER, but ReadMessage returned another message instead")
	}
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Errorf("expected a normal-closure close frame (code %d), got: %v", websocket.CloseNormalClosure, err)
	}
}

// TestWSHandler_GameOver_ClosesConnectionsAfterDelivery is a regression test
// for a bug found via manual E2E testing (PHASE_1.md Step 14): after a game
// ended, both players' WebSocket connections stayed open indefinitely —
// GAME_OVER was sent, but nothing ever closed the socket. A client sending
// further messages after that got silent non-responses, since
// Manager.HandleMessage's registry lookup fails once finalizeGame has
// unregistered the session, and the resulting error was only logged, never
// reported back to the client.
//
// This test asserts two things, not just one: (1) both connections actually
// receive a close frame, and (2) GAME_OVER is the message immediately before
// it, for both players — not just "eventually closed," but "closed only
// after GAME_OVER was delivered." The second assertion is what actually
// exercises this session's fix: GameSession.CloseConnections is called from
// the same goroutine, immediately after the GAME_OVER send (Manager's
// startEventSubscriber), rather than from Manager.finalizeGame on a
// different goroutine.
func TestWSHandler_GameOver_ClosesConnectionsAfterDelivery(t *testing.T) {
	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	blackID := uuid.NewString()
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	session, _, err := manager.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := manager.JoinGame(ctx, session.ID, blackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}

	whiteConn := dialGame(t, srv, session.ID, whiteID, store.ColorWhite)
	defer whiteConn.Close()
	assertMessageType(t, whiteConn, "GAME_STATE") // WAITING_FOR_PLAYER

	blackConn := dialGame(t, srv, session.ID, blackID, store.ColorBlack)
	defer blackConn.Close()
	assertMessageType(t, blackConn, "GAME_STATE") // Black's connect activates the game

	assertMessageType(t, whiteConn, "OPPONENT_CONNECTED")

	// White resigns. Black wins; either side ending the game exercises the
	// same finalizeGame/CloseConnections path, so which side resigns is not
	// significant to this test.
	if err := whiteConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"RESIGN"}`)); err != nil {
		t.Fatalf("write RESIGN: %v", err)
	}

	// Both players receive GAME_OVER ...
	assertMessageType(t, whiteConn, "GAME_OVER")
	assertMessageType(t, blackConn, "GAME_OVER")

	// ... and, strictly after that, both connections are closed by the server
	// with a normal-closure frame — not left open indefinitely.
	assertConnectionClosedNormally(t, whiteConn)
	assertConnectionClosedNormally(t, blackConn)
}

// TestWSHandler_GameOver_NoGoroutineLeaks closes PHASE_1.md acceptance
// criterion #7 ("No goroutine leaks after a completed game"). See the
// standing pattern documented in CLAUDE.md's Known Sharp Edges for why
// goleak.IgnoreCurrent() is required in a scoped, per-test check like this
// one, rather than a package-wide TestMain-level check.
func TestWSHandler_GameOver_NoGoroutineLeaks(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)

	truncateAll(t)
	manager := newTestManager(t)
	srv := newTestServer(t, manager)

	whiteID := uuid.NewString()
	blackID := uuid.NewString()
	mustCreateUser(t, whiteID)
	mustCreateUser(t, blackID)

	ctx := context.Background()
	session, _, err := manager.CreateGame(ctx, whiteID)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if _, err := manager.JoinGame(ctx, session.ID, blackID); err != nil {
		t.Fatalf("JoinGame: %v", err)
	}

	whiteConn := dialGame(t, srv, session.ID, whiteID, store.ColorWhite)
	assertMessageType(t, whiteConn, "GAME_STATE") // WAITING_FOR_PLAYER

	blackConn := dialGame(t, srv, session.ID, blackID, store.ColorBlack)
	assertMessageType(t, blackConn, "GAME_STATE") // Black's connect activates the game

	assertMessageType(t, whiteConn, "OPPONENT_CONNECTED")

	if err := whiteConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"RESIGN"}`)); err != nil {
		t.Fatalf("write RESIGN: %v", err)
	}

	assertMessageType(t, whiteConn, "GAME_OVER")
	assertMessageType(t, blackConn, "GAME_OVER")

	assertConnectionClosedNormally(t, whiteConn)
	assertConnectionClosedNormally(t, blackConn)

	whiteConn.Close()
	blackConn.Close()
	srv.Close()
}
