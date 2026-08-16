package mmsvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// sweepInterval is how often the queue-timeout sweep scans for stale
// entries (DECISIONS_LOG_PHASE_3.md ADR-036: "matchmaking-service runs its
// own periodic sweep — separate ticker from chess-server's pairing-loop
// ticker"). A fixed constant, not env-configurable — unlike the timeout
// threshold itself (Sweep.timeout, MATCHMAKING_QUEUE_TIMEOUT_SECONDS), the
// scan granularity has no correctness implication a client would ever
// observe (worst case: a timed-out player waits up to one extra tick
// before the sweep notices), so there is no case yet for exposing it as
// its own knob — same "don't build a config surface ahead of demonstrated
// need" instinct this project has applied elsewhere (ADR-014, ADR-016).
const sweepInterval = 5 * time.Second

// Sweep implements ADR-036's queue-starvation timeout: a periodic scan of
// the queue ZSET for members who have waited longer than a configured
// threshold, removing them and reporting MATCHMAKING_FAILED with reason
// QUEUE_TIMEOUT — the one MatchmakingFailureReason value that never
// touches gRPC at all (chess-server never attempted a pairing for a player
// who timed out in the queue, so it has nothing to report; see
// translateFailureReason's doc comment in reportserver.go for the
// gRPC-scoped counterpart this generates natively instead of translating).
type Sweep struct {
	redis   *redis.Client
	queue   *Queue
	hub     *Hub
	timeout time.Duration
}

// NewSweep constructs a Sweep. timeout is
// MATCHMAKING_QUEUE_TIMEOUT_SECONDS — how long a player may wait in the
// queue before being removed and notified.
func NewSweep(redisClient *redis.Client, queue *Queue, hub *Hub, timeout time.Duration) *Sweep {
	return &Sweep{redis: redisClient, queue: queue, hub: hub, timeout: timeout}
}

// Start launches the sweep goroutine. Mirrors
// internal/matchmaking.PairingLoop.Start's exact ticker-plus-done-channel
// shape — same "stop blocks until the in-flight tick finishes" guarantee,
// for the same reason: a stop() call during shutdown must not race an
// in-flight tick's Redis writes.
func (sw *Sweep) Start(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sw.tick(ctx)
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// tick finds every queue member whose score (the enqueue Unix timestamp —
// see Queue.Enqueue) is older than sw.timeout, removes them, and reports
// QUEUE_TIMEOUT for each. Uses ZRANGEBYSCORE's max bound rather than
// computing an exact cutoff once and diffing in Go, so a member that ages
// past the threshold mid-tick is simply picked up on the next tick instead
// of racing this one — no correctness requirement for exact-boundary
// precision here.
func (sw *Sweep) tick(ctx context.Context) {
	cutoff := time.Now().Add(-sw.timeout).Unix()

	staleUserIDs, err := sw.redis.ZRangeByScore(ctx, queueKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: fmt.Sprintf("%d", cutoff),
	}).Result()
	if err != nil {
		slog.Error("Sweep.tick: ZRANGEBYSCORE failed", "queueKey", queueKey, "error", err)
		return
	}
	if len(staleUserIDs) == 0 {
		return
	}

	for _, userID := range staleUserIDs {
		// ZREM each individually rather than one call for the whole batch:
		// a member could already be gone by the time this loop reaches it
		// (paired by some instance's pairing-loop tick between this
		// sweep's ZRANGEBYSCORE read and this ZREM) — ZREM on an absent
		// member is a harmless no-op either way, but batching would not
		// change that; individual calls keep the per-member error handling
		// below simple, and this is a background loop with no latency
		// budget worth optimizing for.
		if err := sw.redis.ZRem(ctx, queueKey, userID).Err(); err != nil {
			slog.Error("Sweep.tick: ZREM failed", "userID", userID, "error", err)
			continue
		}

		if err := sw.queue.SetResult(ctx, userID, matchmakingResult{
			Status: "failed",
			Reason: "QUEUE_TIMEOUT",
		}); err != nil {
			slog.Error("Sweep.tick: SetResult failed", "userID", userID, "error", err)
		}
		sw.hub.Notify(userID, EventMatchmakingFailed, matchmakingFailedData{Reason: "QUEUE_TIMEOUT"})

		slog.Info("Sweep.tick: player removed from queue, timed out", "userID", userID)
	}
}
