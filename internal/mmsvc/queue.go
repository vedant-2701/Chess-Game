// Package mmsvc implements matchmaking-service's own HTTP surface — the
// separate deployable PHASE_3.md Step 4 describes, distinct from
// chess-server's internal/matchmaking package (which contains only
// chess-server's half: the pairing loop and gRPC client). This package owns
// queue intake today and will grow SSE notification, the queue-timeout
// sweep, and the MatchReportService gRPC server across the rest of Step 4's
// checklist.
//
// Deliberately does not import internal/game or internal/store
// (DECISIONS_LOG_PHASE_3.md ADR-032: "matchmaking-service never talks to
// GameStore, GameRegistry, or the Redis ownership keys") — it reads the
// matchmaking_active_game:{userID} marker as a raw Redis key with a locally
// defined JSON shape (activeGameMarker below), not by importing
// internal/game.ActiveGameMarker, so this package has zero compile-time
// coupling to chess-server's internals. Same reasoning as redis.go's
// duplicated NewRedisClient.
package mmsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// queueKey mirrors internal/matchmaking's identical constant — duplicated,
// not shared, deliberately: the two packages access the same Redis key by
// contract (PHASE_3.md's "Queue Data Structure" section is the shared
// source of truth for the key name), not by importing a common symbol,
// since matchmaking-service and chess-server's internal/matchmaking package
// must never depend on each other.
const queueKey = "matchmaking:queue:10+0"

func activeGameMarkerKey(userID string) string {
	return "matchmaking_active_game:" + userID
}

// activeGameMarker mirrors internal/game.ActiveGameMarker's JSON shape
// exactly (DECISIONS_LOG_PHASE_3.md ADR-037: camelCase tags, four fields) —
// a local copy, not an import, per this file's package doc comment.
type activeGameMarker struct {
	GameID        string `json:"gameID"`
	ConnectToken  string `json:"connectToken"`
	InstanceLabel string `json:"instanceLabel"`
	WSPath        string `json:"wsPath"`
}

// Queue wraps matchmaking-service's Redis operations: the ADR-037
// active-game check and idempotent enqueue for POST /matchmaking/queue,
// voluntary dequeue for DELETE /matchmaking/queue (ADR-038), and the
// matchmaking-outcome record GET /matchmaking/status reads and the future
// gRPC server (a later Step 4 checklist item) writes. One type, not several
// — all of it is the same *redis.Client, and this package is small enough
// that splitting by endpoint would scatter closely related key-space logic
// across files for no real separation-of-concerns benefit.
type Queue struct {
	redis *redis.Client
}

// NewQueue constructs a Queue over an already-connected, already-verified
// client (see NewRedisClient) — mirrors how GameStore/RedisDirectory wrap an
// already-verified connection rather than managing their own lifecycle.
func NewQueue(redisClient *redis.Client) *Queue {
	return &Queue{redis: redisClient}
}

// ActiveGame returns userID's active-game marker, if chess-server has
// written one (ADR-037's heartbeat-renewed lease key, written only by
// chess-server at CreateGame/JoinGame/CreateMatchedGame success). ok is
// false if no marker is currently set — the common case, and the case that
// allows POST /matchmaking/queue to proceed.
func (q *Queue) ActiveGame(ctx context.Context, userID string) (marker activeGameMarker, ok bool, err error) {
	val, getErr := q.redis.Get(ctx, activeGameMarkerKey(userID)).Result()
	if errors.Is(getErr, redis.Nil) {
		return activeGameMarker{}, false, nil
	}
	if getErr != nil {
		return activeGameMarker{}, false, fmt.Errorf("mmsvc.Queue.ActiveGame userID=%s: %w", userID, getErr)
	}
	if unmarshalErr := json.Unmarshal([]byte(val), &marker); unmarshalErr != nil {
		return activeGameMarker{}, false, fmt.Errorf("mmsvc.Queue.ActiveGame userID=%s: unmarshal: %w", userID, unmarshalErr)
	}
	return marker, true, nil
}

// Enqueue adds userID to the queue at the current time's score, via
// ZADD NX — the actual idempotency mechanism for a retried/duplicate call
// (PHASE_3.md's Queue Data Structure section: "ZADD ... NX — idempotent, so
// a retried/duplicate call never resets queue position"). This is why
// POST /matchmaking/queue does not need a separate "already queued" read
// before writing (PHASE_3.md's Challenge 4 lists ZSCORE as one of two
// decided check-before-enqueue facts, but NX already makes the write itself
// safely idempotent without it — a second read here would be a redundant
// round trip, not additional correctness). added reports whether this call
// actually inserted a new member (true) or found the member already
// present (false, NX no-op); callers use this only for logging — the
// client-visible contract is identical either way (PHASE_3.md: "still
// returns a fresh token").
func (q *Queue) Enqueue(ctx context.Context, userID string) (added bool, err error) {
	n, err := q.redis.ZAddNX(ctx, queueKey, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: userID,
	}).Result()
	if err != nil {
		return false, fmt.Errorf("mmsvc.Queue.Enqueue userID=%s: %w", userID, err)
	}
	return n > 0, nil
}

// Dequeue removes userID from the queue (DELETE /matchmaking/queue's
// voluntary-cancel path, ADR-038). Idempotent by nature of ZREM itself —
// removing a member that is not present (never queued, already matched and
// popped, or already timed out) is a no-op, not an error — DELETE's
// contract does not distinguish "you left the queue" from "you weren't in
// it," matching PHASE_3.md's single 200 response for this endpoint.
func (q *Queue) Dequeue(ctx context.Context, userID string) error {
	if err := q.redis.ZRem(ctx, queueKey, userID).Err(); err != nil {
		return fmt.Errorf("mmsvc.Queue.Dequeue userID=%s: %w", userID, err)
	}
	return nil
}

// resultKey namespaces the matchmaking-outcome record GET
// /matchmaking/status reads and the gRPC server (a later Step 4 checklist
// item, ReportMatchCreated/ReportMatchmakingFailed) writes. A distinct key
// space from activeGameMarkerKey and the queue ZSET itself — three
// different facts about a player, three different keys, none of them
// derivable from another.
func resultKey(userID string) string {
	return "matchmaking_result:" + userID
}

// resultTTL bounds how long a written match outcome stays queryable via
// GET /matchmaking/status after the gRPC server writes it — long enough to
// comfortably outlast MatchmakingClaimsTTL (10s) plus whatever polling
// delay a client's fallback path uses, short enough that a stale outcome
// for a long-since-requeued userID does not linger indefinitely. Not an
// ADR-tracked constant — no ADR fixes this number; revisit if status
// polling in practice needs a longer window.
const resultTTL = 5 * time.Minute

// matchmakingResult is the outcome record GET /matchmaking/status reads.
// Status is either "matched" or "failed" (never "waiting" — that state is
// the absence of a record, see Handler.Status). omitempty on every other
// field: a "matched" result never carries Reason, a "failed" result never
// carries the connect fields, and the JSON shape should reflect that rather
// than spraying empty strings.
type matchmakingResult struct {
	Status        string `json:"status"`
	GameID        string `json:"gameID,omitempty"`
	ConnectToken  string `json:"connectToken,omitempty"`
	InstanceLabel string `json:"instanceLabel,omitempty"`
	WSPath        string `json:"wsPath,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// SetResult writes userID's matchmaking outcome. Not called anywhere in
// this commit — the gRPC server that will call it (ReportMatchCreated /
// ReportMatchmakingFailed) is a later, separate Step 4 checklist item — but
// defined now alongside GetResult so the two form one complete, reviewable
// contract rather than half of one landing now and the other half assumed
// later.
func (q *Queue) SetResult(ctx context.Context, userID string, result matchmakingResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("mmsvc.Queue.SetResult userID=%s: marshal: %w", userID, err)
	}
	if err := q.redis.Set(ctx, resultKey(userID), payload, resultTTL).Err(); err != nil {
		return fmt.Errorf("mmsvc.Queue.SetResult userID=%s: %w", userID, err)
	}
	return nil
}

// GetResult reads userID's matchmaking outcome, if one has been recorded.
// ok is false if no outcome exists yet — Handler.Status treats that as
// "waiting," not an error.
func (q *Queue) GetResult(ctx context.Context, userID string) (result matchmakingResult, ok bool, err error) {
	val, getErr := q.redis.Get(ctx, resultKey(userID)).Result()
	if errors.Is(getErr, redis.Nil) {
		return matchmakingResult{}, false, nil
	}
	if getErr != nil {
		return matchmakingResult{}, false, fmt.Errorf("mmsvc.Queue.GetResult userID=%s: %w", userID, getErr)
	}
	if unmarshalErr := json.Unmarshal([]byte(val), &result); unmarshalErr != nil {
		return matchmakingResult{}, false, fmt.Errorf("mmsvc.Queue.GetResult userID=%s: unmarshal: %w", userID, unmarshalErr)
	}
	return result, true, nil
}
