//go:build integration

package mmsvc

import (
	"context"
	"testing"
	"time"
)

func TestQueue_Enqueue_IdempotentViaNX(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	added1, err := q.Enqueue(ctx, "user-1")
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if !added1 {
		t.Error("expected added=true on first Enqueue")
	}

	score1, err := testRedisClient.ZScore(ctx, queueKey, "user-1").Result()
	if err != nil {
		t.Fatalf("ZScore after first Enqueue: %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // cross a distinguishable Unix-second boundary

	added2, err := q.Enqueue(ctx, "user-1")
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if added2 {
		t.Error("expected added=false on second Enqueue (NX no-op)")
	}

	score2, err := testRedisClient.ZScore(ctx, queueKey, "user-1").Result()
	if err != nil {
		t.Fatalf("ZScore after second Enqueue: %v", err)
	}
	if score1 != score2 {
		t.Errorf("score changed on idempotent retry: first=%v second=%v — NX should have left the original position untouched", score1, score2)
	}
}

func TestQueue_Dequeue_RemovesMember(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, "user-2"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := q.Dequeue(ctx, "user-2"); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	card, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if card != 0 {
		t.Errorf("expected empty queue after Dequeue, got %d members", card)
	}
}

func TestQueue_Dequeue_AbsentMemberIsNoop(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	// Never enqueued at all — Dequeue must not error (see its own doc
	// comment: idempotent by nature of ZREM itself).
	if err := q.Dequeue(ctx, "never-queued"); err != nil {
		t.Errorf("expected no error for an absent member, got: %v", err)
	}
}

func TestQueue_ActiveGame_AbsentMarker(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	_, ok, err := q.ActiveGame(ctx, "no-marker-user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no marker is set")
	}
}

func TestQueue_ActiveGame_PresentMarker(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	// Writes the marker exactly as chess-server's internal/game/directory.go
	// would (ADR-037's JSON shape, PlayerToken per PHASE_3_DESIGN_NOTES.md
	// §18's rename) — raw JSON, not a call into any chess-server code,
	// matching this package's own "reads this key without importing
	// internal/game" design (queue.go's own doc comment).
	const raw = `{"gameID":"g-1","playerToken":"tok-1","instanceLabel":"server1","wsPath":"/connect/server1"}`
	if err := testRedisClient.Set(ctx, activeGameMarkerKey("marker-user"), raw, 0).Err(); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	marker, ok, err := q.ActiveGame(ctx, "marker-user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if marker.GameID != "g-1" || marker.PlayerToken != "tok-1" || marker.InstanceLabel != "server1" || marker.WSPath != "/connect/server1" {
		t.Errorf("unexpected marker: %+v", marker)
	}
}

func TestQueue_SetResult_GetResult_RoundTrip(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	want := matchmakingResult{
		Status:        "matched",
		GameID:        "g-2",
		ConnectToken:  "tok-2",
		InstanceLabel: "server2",
		WSPath:        "/connect/server2",
	}
	if err := q.SetResult(ctx, "result-user", want); err != nil {
		t.Fatalf("SetResult: %v", err)
	}

	got, ok, err := q.GetResult(ctx, "result-user")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != want {
		t.Errorf("GetResult = %+v, want %+v", got, want)
	}
}

func TestQueue_GetResult_AbsentIsNotAnError(t *testing.T) {
	flushTestRedisDB(t)
	q := NewQueue(testRedisClient)
	ctx := context.Background()

	_, ok, err := q.GetResult(ctx, "no-result-user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no result has been recorded — Handler.Status treats this as \"waiting\", not an error")
	}
}
