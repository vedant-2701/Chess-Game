//go:build integration

package mmsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vedant-2701/chess/internal/auth"
)

const testJWTSecret = "mmsvc-test-secret-not-for-prod"

// newTestServer wires a full Handler behind NewRouter exactly as
// cmd/matchmaking-service/main.go does — same pattern as
// internal/api/game_handler_test.go's newTestGameServer.
func newTestServer(t *testing.T) (*httptest.Server, *Queue) {
	t.Helper()
	q := NewQueue(testRedisClient)
	h := NewHub()
	handler := NewHandler(q, h, testJWTSecret, auth.DefaultMatchmakingClaimsTTL)
	router := NewRouter(handler)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, q
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

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

func decodeErrBody(t *testing.T, resp *http.Response) errorDetail {
	t.Helper()
	var env struct {
		Error errorDetail `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return env.Error
}

func mustSignMatchmakingToken(t *testing.T, userID string) string {
	t.Helper()
	token, err := auth.SignMatchmakingToken(auth.MatchmakingClaims{UserID: userID}, testJWTSecret)
	if err != nil {
		t.Fatalf("SignMatchmakingToken: %v", err)
	}
	return token
}

// --- Queue (POST /matchmaking/queue) ---

func TestHandler_Queue_Success(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	userID := uuid.NewString()
	resp := postJSON(t, srv, "/matchmaking/queue", map[string]string{"userID": userID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[queueResponseData](t, resp)
	if data.MatchmakingToken == "" {
		t.Fatal("expected a non-empty matchmakingToken")
	}

	claims, err := auth.VerifyMatchmakingToken(data.MatchmakingToken, testJWTSecret)
	if err != nil {
		t.Fatalf("VerifyMatchmakingToken: %v", err)
	}
	if claims.UserID != userID {
		t.Errorf("token userID = %q, want %q", claims.UserID, userID)
	}

	if _, err := testRedisClient.ZScore(context.Background(), queueKey, userID).Result(); err != nil {
		t.Errorf("expected %s to be present in the queue: %v", userID, err)
	}
}

func TestHandler_Queue_AlreadyInActiveGame(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)
	ctx := context.Background()

	userID := uuid.NewString()
	const raw = `{"gameID":"g-active","playerToken":"tok","instanceLabel":"server1","wsPath":"/connect/server1"}`
	if err := testRedisClient.Set(ctx, activeGameMarkerKey(userID), raw, 0).Err(); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	resp := postJSON(t, srv, "/matchmaking/queue", map[string]string{"userID": userID})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	errDetail := decodeErrBody(t, resp)
	if errDetail.Code != errCodeAlreadyInActiveGame {
		t.Errorf("code = %q, want %q", errDetail.Code, errCodeAlreadyInActiveGame)
	}
	if errDetail.ExistingGame == nil || errDetail.ExistingGame.GameID != "g-active" {
		t.Errorf("expected existingGame.gameID = g-active, got %+v", errDetail.ExistingGame)
	}

	// Must NOT have been enqueued despite the rejection.
	if _, err := testRedisClient.ZScore(ctx, queueKey, userID).Result(); err == nil {
		t.Error("expected the player to NOT be in the queue after a 409")
	}
}

func TestHandler_Queue_IdempotentSecondCallReturnsFreshToken(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	userID := uuid.NewString()
	first := postJSON(t, srv, "/matchmaking/queue", map[string]string{"userID": userID})
	firstData := decodeData[queueResponseData](t, first)

	time.Sleep(1100 * time.Millisecond)

	second := postJSON(t, srv, "/matchmaking/queue", map[string]string{"userID": userID})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", second.StatusCode)
	}
	secondData := decodeData[queueResponseData](t, second)

	if secondData.MatchmakingToken == firstData.MatchmakingToken {
		t.Error("expected a fresh token on the second call (different IssuedAt), not the identical token")
	}

	card, err := testRedisClient.ZCard(context.Background(), queueKey).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if card != 1 {
		t.Errorf("expected exactly one queue entry after two calls for the same user, got %d", card)
	}
}

func TestHandler_Queue_MalformedUserID(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	resp := postJSON(t, srv, "/matchmaking/queue", map[string]string{"userID": "not-a-uuid"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	errDetail := decodeErrBody(t, resp)
	if errDetail.Code != errCodeInvalidRequest {
		t.Errorf("code = %q, want %q", errDetail.Code, errCodeInvalidRequest)
	}
}

// --- Status (GET /matchmaking/status) ---

func TestHandler_Status_Waiting(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	token := mustSignMatchmakingToken(t, uuid.NewString())
	resp, err := http.Get(srv.URL + "/matchmaking/status?token=" + token)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeData[statusResponseData](t, resp)
	if data.Status != "waiting" {
		t.Errorf("status = %q, want waiting", data.Status)
	}
}

func TestHandler_Status_Matched(t *testing.T) {
	flushTestRedisDB(t)
	srv, q := newTestServer(t)
	ctx := context.Background()

	userID := uuid.NewString()
	if err := q.SetResult(ctx, userID, matchmakingResult{
		Status: "matched", GameID: "g-1", ConnectToken: "tok", InstanceLabel: "server1", WSPath: "/connect/server1",
	}); err != nil {
		t.Fatalf("SetResult: %v", err)
	}

	token := mustSignMatchmakingToken(t, userID)
	resp, err := http.Get(srv.URL + "/matchmaking/status?token=" + token)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	data := decodeData[statusResponseData](t, resp)
	if data.Status != "matched" || data.GameID != "g-1" {
		t.Errorf("unexpected data: %+v", data)
	}
}

// --- Cancel (DELETE /matchmaking/queue) ---

func TestHandler_Cancel_RemovesFromQueue(t *testing.T) {
	flushTestRedisDB(t)
	srv, q := newTestServer(t)
	ctx := context.Background()

	userID := uuid.NewString()
	if _, err := q.Enqueue(ctx, userID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	token := mustSignMatchmakingToken(t, userID)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/matchmaking/queue?token="+token, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	card, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if card != 0 {
		t.Errorf("expected empty queue after cancel, got %d members", card)
	}
}

// --- Auth guard shared by Stream/Status/Cancel ---

func TestHandler_MissingToken_Unauthorized(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/matchmaking/status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHandler_InvalidToken_Unauthorized(t *testing.T) {
	flushTestRedisDB(t)
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/matchmaking/status?token=not-a-real-jwt")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}
