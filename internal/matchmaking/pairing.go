// Package matchmaking is chess-server's half of Phase 3's server-picks
// pairing model (PHASE_3_DESIGN_NOTES.md §1-§2, DECISIONS_LOG_PHASE_3.md
// ADR-032). matchmaking-service (a separate deployable, Phase 3 Step 4, not
// implemented in this package) owns queue intake and match notification;
// this package owns dequeuing and game creation only.
package matchmaking

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/vedant-2701/chess/internal/game"
)

// queueKey is the Redis sorted-set queue's key. Single time control only in
// Phase 3 (PHASE_3.md Scope: "Single queue: 10+0 time control only") —
// Phase 4's multiple-time-control support will need to generalize this into
// a per-time-control key, not add a second hardcoded constant next to this
// one.
const queueKey = "matchmaking:queue:10+0"

// defaultMaxCreateAttempts bounds how many times pairAndReport retries
// Manager.CreateMatchedGame for a single popped pair before declaring the
// attempt a confirmed, terminal failure (PHASE_3_DESIGN_NOTES.md §6's
// Reliability section) — re-enqueuing both players and reporting the
// failure. Every retry reuses the same matchmakingRequestID
// (DECISIONS_LOG_PHASE_3.md ADR-034) — that reuse, not this number, is what
// makes retrying safe; this bound only exists so an instance doesn't retry a
// genuinely broken insert (e.g. a sustained Postgres outage) forever while
// two real players wait.
const defaultMaxCreateAttempts = 3

// MatchReporter reports a pairing attempt's outcome to matchmaking-service
// over gRPC (DECISIONS_LOG_PHASE_3.md ADR-033/ADR-034,
// PHASE_3_DESIGN_NOTES.md §6). Defined as an interface, not a concrete gRPC
// client type, so PairingLoop's tick logic is testable without a running
// matchmaking-service or the generated protobuf stubs — MatchReportService's
// generated client code (proto/matchmakingv1, Makefile's `make proto`
// target) has not been generated as of this package's construction; a
// concrete gRPC-backed implementation of this interface is Phase 3 Step 3's
// separate "gRPC client" checklist item, to be wired in without any change
// to PairingLoop itself once it exists.
//
// Both methods are best-effort/non-fatal from PairingLoop's perspective —
// PHASE_3_DESIGN_NOTES.md §6's Reliability section is explicit that the game
// already exists in Postgres (or doesn't, for the failure case) by the time
// either of these fires; a failed *report* must never roll back or retry
// game creation itself. Implementations are expected to retry internally
// with bounded backoff and log-and-return on final exhaustion — PairingLoop
// does not retry on top of whatever the implementation already did.
type MatchReporter interface {
	// ReportMatchCreated reports a successful CreateMatchedGame.
	ReportMatchCreated(ctx context.Context, gameID, whiteUserID, blackUserID, whiteToken, blackToken, instanceLabel string) error

	// ReportMatchmakingFailed reports a confirmed, terminal CreateMatchedGame
	// failure (retries exhausted) for a specific pair.
	ReportMatchmakingFailed(ctx context.Context, whiteUserID, blackUserID string) error
}

// PairingLoop runs chess-server's half of the server-picks pairing model:
// every chess-server instance runs its own copy of this loop against the
// same Redis queue, contending directly via ZPOPMIN's atomicity
// (PHASE_3.md's Challenge 1) rather than coordinating with other instances
// or waiting for matchmaking-service to assign anything.
type PairingLoop struct {
	redis             *redis.Client
	manager           *game.Manager
	reporter          MatchReporter
	interval          time.Duration
	instanceID        string
	maxCreateAttempts int
}

// NewPairingLoop constructs a PairingLoop. redisClient must be an
// already-connected, already-verified *redis.Client — the same one used
// elsewhere in this process (game.NewRedisClient), per Phase 3 Step 3's
// "Redis client wiring decision": this constructor's signature enforces
// reuse of the existing connection by taking an already-built client rather
// than a redisAddr string of its own; internal/matchmaking never opens a
// second Redis connection pool.
//
// instanceID must be the same value passed to game.NewManager for manager —
// PairingLoop reports it verbatim as instanceLabel (Manager.ResolveGame's
// own instanceLabel return value is literally m.instanceID under a
// different name at that call site; there is no separate "label" concept
// anywhere in this codebase to derive it from instead).
func NewPairingLoop(redisClient *redis.Client, manager *game.Manager, reporter MatchReporter, interval time.Duration, instanceID string) *PairingLoop {
	return &PairingLoop{
		redis:             redisClient,
		manager:           manager,
		reporter:          reporter,
		interval:          interval,
		instanceID:        instanceID,
		maxCreateAttempts: defaultMaxCreateAttempts,
	}
}

// Start launches the pairing-loop goroutine, ticking at the configured
// interval (MATCHMAKING_PAIRING_INTERVAL_MS). Mirrors
// Manager.StartHeartbeat's Start/Stop shape exactly
// (internal/game/heartbeat.go) — same ticker-plus-done-channel pattern,
// same "stop blocks until the in-flight tick finishes" guarantee, for the
// same reason: a stop() call during shutdown must not race an in-flight
// tick's Redis/Postgres writes.
//
// Unlike StartHeartbeat, there is no equivalent "release entries on stop"
// step — a tick either completes a pairing attempt fully
// (CreateMatchedGame + report) or re-enqueues on confirmed failure; there is
// no per-instance Redis state this loop itself owns that would need
// releasing on shutdown. ZPOPMIN's removal from the queue is already
// permanent, not a lease.
func (l *PairingLoop) Start(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				l.tick(ctx)
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// tick runs one pairing attempt: a ZCARD guard, then ZPOPMIN 2, and — on a
// successful pop — hands the pair to pairAndReport. Errors from individual
// steps are logged, never propagated or panicked on — this is a background
// loop with no caller waiting on a return value, matching
// Manager.heartbeatTick's own error-handling shape exactly.
func (l *PairingLoop) tick(ctx context.Context) {
	count, err := l.redis.ZCard(ctx, queueKey).Result()
	if err != nil {
		slog.Error("PairingLoop.tick: ZCARD failed", "queueKey", queueKey, "error", err)
		return
	}
	if count < 2 {
		return
	}

	pair, err := l.redis.ZPopMin(ctx, queueKey, 2).Result()
	if err != nil {
		slog.Error("PairingLoop.tick: ZPOPMIN failed", "queueKey", queueKey, "error", err)
		return
	}
	if len(pair) < 2 {
		// PHASE_3.md's Challenge 1: another instance's pairing loop (or a
		// concurrent tick racing the ZCARD check above) got there first, or
		// the queue simply emptied between ZCARD and ZPOPMIN. Not an error —
		// this is one of ZPOPMIN's two entirely expected outcomes, exactly
		// the race its atomicity exists to resolve.
		return
	}

	whiteUserID, ok := pair[0].Member.(string)
	if !ok {
		slog.Error("PairingLoop.tick: unexpected ZPOPMIN member type",
			"member", pair[0].Member, "type", fmt.Sprintf("%T", pair[0].Member))
		return
	}
	blackUserID, ok := pair[1].Member.(string)
	if !ok {
		slog.Error("PairingLoop.tick: unexpected ZPOPMIN member type",
			"member", pair[1].Member, "type", fmt.Sprintf("%T", pair[1].Member))
		return
	}

	l.pairAndReport(ctx, whiteUserID, pair[0].Score, blackUserID, pair[1].Score)
}

// pairAndReport runs Manager.CreateMatchedGame for a popped pair, with
// bounded retries sharing one matchmakingRequestID
// (PHASE_3_DESIGN_NOTES.md §8, DECISIONS_LOG_PHASE_3.md ADR-034) — reusing
// the same requestID across retries, not minting a fresh one per attempt, is
// the entire point: it is what makes retrying after an ambiguous failure
// safe rather than a double-booking risk. On success, reports via gRPC. On
// confirmed, terminal failure (all attempts exhausted), re-enqueues both
// players at their ORIGINAL scores (preserving queue fairness —
// PHASE_3_DESIGN_NOTES.md §5's failure branch, DECIDED in §8: chess-server
// owns re-enqueue directly) and reports the failure via gRPC.
func (l *PairingLoop) pairAndReport(ctx context.Context, whiteUserID string, whiteScore float64, blackUserID string, blackScore float64) {
	requestID := uuid.New().String()

	var (
		session                *game.GameSession
		whiteToken, blackToken string
		err                    error
	)
	for attempt := 1; attempt <= l.maxCreateAttempts; attempt++ {
		session, whiteToken, blackToken, err = l.manager.CreateMatchedGame(ctx, whiteUserID, blackUserID, requestID)
		if err == nil {
			break
		}
		slog.Warn("PairingLoop: CreateMatchedGame attempt failed",
			"whiteUserID", whiteUserID, "blackUserID", blackUserID, "requestID", requestID,
			"attempt", attempt, "maxAttempts", l.maxCreateAttempts, "error", err)
	}

	if err != nil {
		slog.Error("PairingLoop: CreateMatchedGame failed after all attempts — re-enqueuing",
			"whiteUserID", whiteUserID, "blackUserID", blackUserID, "requestID", requestID,
			"maxAttempts", l.maxCreateAttempts, "error", err)

		l.reenqueue(ctx, whiteUserID, whiteScore, blackUserID, blackScore)

		if reportErr := l.reporter.ReportMatchmakingFailed(ctx, whiteUserID, blackUserID); reportErr != nil {
			slog.Error("PairingLoop: ReportMatchmakingFailed failed",
				"whiteUserID", whiteUserID, "blackUserID", blackUserID, "error", reportErr)
		}
		return
	}

	if reportErr := l.reporter.ReportMatchCreated(ctx, session.ID, whiteUserID, blackUserID, whiteToken, blackToken, l.instanceID); reportErr != nil {
		slog.Error("PairingLoop: ReportMatchCreated failed",
			"gameID", session.ID, "whiteUserID", whiteUserID, "blackUserID", blackUserID, "error", reportErr)
	}
}

// reenqueue re-adds both players to the queue at their ORIGINAL scores —
// preserving queue position/fairness, since a server-side failure was not
// their fault (PHASE_3_DESIGN_NOTES.md §5's failure branch). Chess-server
// owns this directly, not matchmaking-service (DECIDED,
// PHASE_3_DESIGN_NOTES.md §8): the matchmaking_request_id idempotency column
// already closes the ambiguous-failure double-booking hazard that decision
// depended on. Plain ZADD, not ZADD NX: NX is specifically
// POST /matchmaking/queue's mechanism (matchmaking-service) for making a
// client's own repeat call idempotent against resetting their queue
// position; here, the member is already known absent (ZPOPMIN removed it
// moments ago in this exact tick), so there is no existing position to
// protect, and re-adding an already-absent member is naturally idempotent
// regardless (sorted-set members are unique) — so even an accidental double
// re-enqueue by this loop is safe by construction.
//
// A ZADD failure here is a real, accepted, logged-but-unretried gap: the
// two players are silently lost from the queue with no game to show for it.
// Not retried further — CreateMatchedGame's own bounded retry loop above
// already covers its failure mode; a second retry layer here for a rare
// double-failure (insert AND re-enqueue both failing) trades unbounded
// complexity for a case this project's established risk tolerance (see
// TD-P2-006, ADR-042's own accepted residual) treats as acceptable to flag
// rather than engineer around.
func (l *PairingLoop) reenqueue(ctx context.Context, whiteUserID string, whiteScore float64, blackUserID string, blackScore float64) {
	if err := l.redis.ZAdd(ctx, queueKey,
		redis.Z{Score: whiteScore, Member: whiteUserID},
		redis.Z{Score: blackScore, Member: blackUserID},
	).Err(); err != nil {
		slog.Error("PairingLoop.reenqueue: ZADD failed — players lost from queue",
			"whiteUserID", whiteUserID, "blackUserID", blackUserID, "error", err)
	}
}
