//go:build integration

package mmsvc

import (
	"context"
	"testing"
	"time"

	matchmakingv1 "github.com/vedant-2701/chess/proto/matchmakingv1"
)

func TestReportServer_ReportMatchCreated_NotifiesBothPlayersAndSetsResult(t *testing.T) {
	flushTestRedisDB(t)
	ctx := context.Background()
	q := NewQueue(testRedisClient)
	h := NewHub()
	rs := NewReportServer(testRedisClient, q, h)

	whiteCh := h.register("white-user")
	defer h.unregister("white-user", whiteCh)
	blackCh := h.register("black-user")
	defer h.unregister("black-user", blackCh)

	req := &matchmakingv1.MatchCreatedRequest{
		GameId:            "game-1",
		WhiteUserId:       "white-user",
		BlackUserId:       "black-user",
		WhitePlayerToken:  "white-player-claims-token",
		BlackPlayerToken:  "black-player-claims-token",
		WhiteConnectToken: "white-connect-claims-token",
		BlackConnectToken: "black-connect-claims-token",
		InstanceLabel:     "server1",
	}

	resp, err := rs.ReportMatchCreated(ctx, req)
	if err != nil {
		t.Fatalf("ReportMatchCreated: %v", err)
	}
	if !resp.GetAcknowledged() {
		t.Error("expected Acknowledged=true")
	}

	// Both token pairs must be relayed unchanged, per-player, not swapped or
	// shared — the load-bearing "opaque pass-through" behavior described in
	// ReportMatchCreated's doc comment (PHASE_3_DESIGN_NOTES.md §18).
	select {
	case evt := <-whiteCh:
		data := evt.data.(matchFoundData)
		if data.ConnectToken != "white-connect-claims-token" {
			t.Errorf("white got connectToken %q, want white-connect-claims-token", data.ConnectToken)
		}
		if data.PlayerToken != "white-player-claims-token" {
			t.Errorf("white got playerToken %q, want white-player-claims-token", data.PlayerToken)
		}
		if data.WSPath != "/connect/server1" {
			t.Errorf("white got wsPath %q, want /connect/server1", data.WSPath)
		}
	case <-time.After(time.Second):
		t.Fatal("white-user did not receive MATCH_FOUND")
	}
	select {
	case evt := <-blackCh:
		data := evt.data.(matchFoundData)
		if data.ConnectToken != "black-connect-claims-token" {
			t.Errorf("black got connectToken %q, want black-connect-claims-token", data.ConnectToken)
		}
		if data.PlayerToken != "black-player-claims-token" {
			t.Errorf("black got playerToken %q, want black-player-claims-token", data.PlayerToken)
		}
	case <-time.After(time.Second):
		t.Fatal("black-user did not receive MATCH_FOUND")
	}

	whiteResult, ok, err := q.GetResult(ctx, "white-user")
	if err != nil || !ok {
		t.Fatalf("GetResult(white-user): ok=%v err=%v", ok, err)
	}
	if whiteResult.Status != "matched" || whiteResult.GameID != "game-1" {
		t.Errorf("unexpected white result: %+v", whiteResult)
	}
	if whiteResult.ConnectToken != "white-connect-claims-token" || whiteResult.PlayerToken != "white-player-claims-token" {
		t.Errorf("unexpected white result tokens: %+v", whiteResult)
	}
}

func TestReportServer_ReportMatchCreated_DedupSkipsSecondDelivery(t *testing.T) {
	flushTestRedisDB(t)
	ctx := context.Background()
	q := NewQueue(testRedisClient)
	h := NewHub()
	rs := NewReportServer(testRedisClient, q, h)

	ch := h.register("dedup-user")
	defer h.unregister("dedup-user", ch)

	req := &matchmakingv1.MatchCreatedRequest{
		GameId:            "game-dedup",
		WhiteUserId:       "dedup-user",
		BlackUserId:       "other-user",
		WhitePlayerToken:  "player-tok-1",
		BlackPlayerToken:  "player-tok-2",
		WhiteConnectToken: "connect-tok-1",
		BlackConnectToken: "connect-tok-2",
		InstanceLabel:     "server1",
	}

	if _, err := rs.ReportMatchCreated(ctx, req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Drain the first event so the second call's (lack of) delivery is
	// unambiguous.
	<-ch

	// Simulates chess-server retrying after a lost ACK — same gameID, same
	// request.
	resp2, err := rs.ReportMatchCreated(ctx, req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !resp2.GetAcknowledged() {
		t.Error("expected the duplicate call to still be acknowledged")
	}

	select {
	case evt := <-ch:
		t.Errorf("expected NO second MATCH_FOUND delivery (dedup), got %+v", evt)
	default:
		// correct — deduped
	}
}

func TestReportServer_ReportMatchmakingFailed_NoDedup_NotifiesBothPlayers(t *testing.T) {
	flushTestRedisDB(t)
	ctx := context.Background()
	q := NewQueue(testRedisClient)
	h := NewHub()
	rs := NewReportServer(testRedisClient, q, h)

	whiteCh := h.register("fail-white")
	defer h.unregister("fail-white", whiteCh)
	blackCh := h.register("fail-black")
	defer h.unregister("fail-black", blackCh)

	req := &matchmakingv1.MatchmakingFailedRequest{
		WhiteUserId: "fail-white",
		BlackUserId: "fail-black",
		Reason:      matchmakingv1.MatchmakingFailureReason_MATCHMAKING_FAILURE_REASON_RETRIES_EXHAUSTED,
	}

	if _, err := rs.ReportMatchmakingFailed(ctx, req); err != nil {
		t.Fatalf("ReportMatchmakingFailed: %v", err)
	}

	for label, ch := range map[string]chan sseEvent{"white": whiteCh, "black": blackCh} {
		select {
		case evt := <-ch:
			data, ok := evt.data.(matchmakingFailedData)
			if !ok || data.Reason != "RETRIES_EXHAUSTED" {
				t.Errorf("%s: unexpected data %+v", label, evt.data)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive MATCHMAKING_FAILED", label)
		}
	}

	// No dedup for this RPC (ADR-033) — calling twice must deliver twice,
	// unlike ReportMatchCreated's dedup above.
	if _, err := rs.ReportMatchmakingFailed(ctx, req); err != nil {
		t.Fatalf("second ReportMatchmakingFailed: %v", err)
	}
	select {
	case <-whiteCh:
		// correct — second delivery arrived
	case <-time.After(time.Second):
		t.Error("expected a second MATCHMAKING_FAILED delivery (no dedup for this RPC)")
	}
}
