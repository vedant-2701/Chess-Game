//go:build integration

package mmsvc

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestSweep_Tick_RemovesStaleMembersOnly(t *testing.T) {
	flushTestRedisDB(t)
	ctx := context.Background()
	q := NewQueue(testRedisClient)
	h := NewHub()

	// Seed one "stale" member (scored directly with a past timestamp,
	// bypassing Queue.Enqueue's time.Now() so the test is deterministic
	// rather than sleep-dependent) and one "fresh" member (scored now).
	staleScore := float64(time.Now().Add(-time.Hour).Unix())
	freshScore := float64(time.Now().Unix())
	if err := testRedisClient.ZAdd(ctx, queueKey,
		redis.Z{Score: staleScore, Member: "stale-user"},
		redis.Z{Score: freshScore, Member: "fresh-user"},
	).Err(); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	staleCh := h.register("stale-user")
	defer h.unregister("stale-user", staleCh)
	freshCh := h.register("fresh-user")
	defer h.unregister("fresh-user", freshCh)

	sw := NewSweep(testRedisClient, q, h, 5*time.Minute) // threshold: shorter than 1h, longer than 0
	sw.tick(ctx)

	members, err := testRedisClient.ZRange(ctx, queueKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("ZRange: %v", err)
	}
	if len(members) != 1 || members[0] != "fresh-user" {
		t.Errorf("expected only fresh-user to remain, got %v", members)
	}

	select {
	case evt := <-staleCh:
		if evt.event != EventMatchmakingFailed {
			t.Errorf("stale-user event = %q, want %q", evt.event, EventMatchmakingFailed)
		}
		data, ok := evt.data.(matchmakingFailedData)
		if !ok || data.Reason != "QUEUE_TIMEOUT" {
			t.Errorf("stale-user data = %+v, want reason QUEUE_TIMEOUT", evt.data)
		}
	default:
		t.Error("expected stale-user to receive a MATCHMAKING_FAILED event")
	}

	select {
	case evt := <-freshCh:
		t.Errorf("fresh-user should not have received any event, got %+v", evt)
	default:
		// correct — nothing sent
	}

	result, ok, err := q.GetResult(ctx, "stale-user")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if !ok || result.Status != "failed" || result.Reason != "QUEUE_TIMEOUT" {
		t.Errorf("expected stale-user's result to be failed/QUEUE_TIMEOUT, got ok=%v result=%+v", ok, result)
	}

	if _, ok, _ := q.GetResult(ctx, "fresh-user"); ok {
		t.Error("expected no result recorded for fresh-user")
	}
}

func TestSweep_Tick_EmptyQueueIsNoop(t *testing.T) {
	flushTestRedisDB(t)
	ctx := context.Background()
	q := NewQueue(testRedisClient)
	h := NewHub()
	sw := NewSweep(testRedisClient, q, h, time.Minute)

	// Must not panic or error against an empty queue.
	sw.tick(ctx)

	card, err := testRedisClient.ZCard(ctx, queueKey).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if card != 0 {
		t.Errorf("expected queue to remain empty, got %d members", card)
	}
}
