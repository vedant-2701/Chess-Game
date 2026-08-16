package mmsvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	matchmakingv1 "github.com/vedant-2701/chess/proto/matchmakingv1"

	"github.com/redis/go-redis/v9"
)

// reportedDedupTTL bounds how long a MatchCreatedRequest's dedup key
// (reported:<gameID>) is remembered — long enough to comfortably outlast
// chess-server's own bounded gRPC retry window
// (internal/matchmaking/reporter.go's grpcMatchReporter:
// defaultReportMaxAttempts=3 with backoff — a few hundred milliseconds to
// low seconds total), short enough not to accumulate unboundedly. Not an
// ADR-tracked constant — no ADR fixes this number.
const reportedDedupTTL = 5 * time.Minute

func reportedKey(gameID string) string {
	return "reported:" + gameID
}

// ReportServer implements matchmakingv1.MatchReportServiceServer
// (DECISIONS_LOG_PHASE_3.md ADR-033) — the gRPC-receiving half of the
// server-picks pairing model. Embeds UnimplementedMatchReportServiceServer
// by value, per the generated code's own forward-compatibility contract
// (matchmaking_grpc.pb.go's testEmbeddedByValue panic guard).
type ReportServer struct {
	matchmakingv1.UnimplementedMatchReportServiceServer

	redis *redis.Client
	queue *Queue
	hub   *Hub
}

// NewReportServer constructs a ReportServer.
func NewReportServer(redisClient *redis.Client, queue *Queue, hub *Hub) *ReportServer {
	return &ReportServer{redis: redisClient, queue: queue, hub: hub}
}

// ReportMatchCreated implements the proto's ReportMatchCreated RPC
// (DECISIONS_LOG_PHASE_3.md ADR-033/ADR-037). Dedups via a short-TTL
// reported:<gameID> key set with SETNX before doing any of the real work —
// covers a chess-server retry after a lost ACK without double-delivering
// MATCH_FOUND or re-writing the result record (ADR-033's Consequences:
// "game_id is the idempotency key... matchmaking-service keeps a short-TTL
// Redis dedup key checked before pushing SSE").
//
// white_token/black_token are opaque PlayerClaims JWT strings, already
// signed by chess-server (DECISIONS_LOG_PHASE_3.md ADR-037's Consequences:
// "connectToken is the player's existing PlayerClaims [24h TTL]" — despite
// the field name matching /resolve's ConnectClaims-shaped response,
// ADR-037 is explicit its actual content is the long-lived PlayerClaims
// token, minted once by chess-server, republished unchanged). This method
// never parses or verifies them, only relays them unchanged into the
// connectToken field of both the result record (Queue.SetResult) and the
// MATCH_FOUND SSE event (Hub.Notify) — same reasoning as this package's
// other refusals to import internal/auth for anything beyond what it
// strictly needs.
func (s *ReportServer) ReportMatchCreated(ctx context.Context, req *matchmakingv1.MatchCreatedRequest) (*matchmakingv1.MatchCreatedResponse, error) {
	isNew, err := s.redis.SetNX(ctx, reportedKey(req.GetGameId()), "1", reportedDedupTTL).Result()
	if err != nil {
		slog.Error("ReportServer.ReportMatchCreated: dedup SETNX failed", "gameID", req.GetGameId(), "error", err)
		return nil, fmt.Errorf("mmsvc.ReportServer.ReportMatchCreated gameID=%s: dedup: %w", req.GetGameId(), err)
	}
	if !isNew {
		// Already processed — a chess-server retry after a lost ACK, not a
		// new event. Acknowledge without redoing the SSE push / result
		// write, so a retry looks identical to chess-server regardless of
		// whether this is the first delivery.
		slog.Debug("ReportServer.ReportMatchCreated: duplicate report, already processed",
			"gameID", req.GetGameId())
		return &matchmakingv1.MatchCreatedResponse{Acknowledged: true}, nil
	}

	wsPath := "/connect/" + req.GetInstanceLabel()

	whiteData := matchFoundData{
		GameID:        req.GetGameId(),
		ConnectToken:  req.GetWhiteToken(),
		InstanceLabel: req.GetInstanceLabel(),
		WSPath:        wsPath,
	}
	blackData := matchFoundData{
		GameID:        req.GetGameId(),
		ConnectToken:  req.GetBlackToken(),
		InstanceLabel: req.GetInstanceLabel(),
		WSPath:        wsPath,
	}

	if setErr := s.recordAndNotifyMatched(ctx, req.GetWhiteUserId(), whiteData); setErr != nil {
		slog.Error("ReportServer.ReportMatchCreated: white SetResult failed",
			"gameID", req.GetGameId(), "userID", req.GetWhiteUserId(), "error", setErr)
	}
	if setErr := s.recordAndNotifyMatched(ctx, req.GetBlackUserId(), blackData); setErr != nil {
		slog.Error("ReportServer.ReportMatchCreated: black SetResult failed",
			"gameID", req.GetGameId(), "userID", req.GetBlackUserId(), "error", setErr)
	}

	return &matchmakingv1.MatchCreatedResponse{Acknowledged: true}, nil
}

func (s *ReportServer) recordAndNotifyMatched(ctx context.Context, userID string, data matchFoundData) error {
	if err := s.queue.SetResult(ctx, userID, matchmakingResult{
		Status:        "matched",
		GameID:        data.GameID,
		ConnectToken:  data.ConnectToken,
		InstanceLabel: data.InstanceLabel,
		WSPath:        data.WSPath,
	}); err != nil {
		return err
	}
	s.hub.Notify(userID, EventMatchFound, data)
	return nil
}

// ReportMatchmakingFailed implements the proto's ReportMatchmakingFailed
// RPC. No dedup key — ADR-033's Consequences are explicit this RPC "has no
// natural key (no game exists yet); accepted at-least-once without dedup
// infrastructure." A duplicate delivery here just overwrites the same
// SetResult value and re-fires Hub.Notify — harmless: SetResult is a plain
// overwrite, not an accumulator, and a client's SSE handler receiving
// MATCHMAKING_FAILED twice for the same reason is not the correctness
// hazard double-delivering MATCH_FOUND for two different concurrent games
// would be.
func (s *ReportServer) ReportMatchmakingFailed(ctx context.Context, req *matchmakingv1.MatchmakingFailedRequest) (*matchmakingv1.MatchmakingFailedResponse, error) {
	reason := translateFailureReason(req.GetReason())

	if err := s.recordAndNotifyFailed(ctx, req.GetWhiteUserId(), reason); err != nil {
		slog.Error("ReportServer.ReportMatchmakingFailed: white SetResult failed",
			"userID", req.GetWhiteUserId(), "error", err)
	}
	if err := s.recordAndNotifyFailed(ctx, req.GetBlackUserId(), reason); err != nil {
		slog.Error("ReportServer.ReportMatchmakingFailed: black SetResult failed",
			"userID", req.GetBlackUserId(), "error", err)
	}

	return &matchmakingv1.MatchmakingFailedResponse{Acknowledged: true}, nil
}

func (s *ReportServer) recordAndNotifyFailed(ctx context.Context, userID string, reason string) error {
	if err := s.queue.SetResult(ctx, userID, matchmakingResult{
		Status: "failed",
		Reason: reason,
	}); err != nil {
		return err
	}
	s.hub.Notify(userID, EventMatchmakingFailed, matchmakingFailedData{Reason: reason})
	return nil
}

// translateFailureReason converts the proto's narrow, gRPC-scoped
// MatchmakingFailureReason enum into matchmaking-service's own broader,
// client-facing string vocabulary (DECISIONS_LOG_PHASE_3.md ADR-036: "the
// proto enum stays scoped to what chess-server can actually experience
// (RETRIES_EXHAUSTED only); matchmaking-service needs its own, broader
// client-facing reason vocabulary... plus QUEUE_TIMEOUT, generated
// natively"). QUEUE_TIMEOUT is generated directly by the sweep goroutine
// (sweep.go), never through this translation — this function only ever
// sees values chess-server can actually send over gRPC.
func translateFailureReason(reason matchmakingv1.MatchmakingFailureReason) string {
	switch reason {
	case matchmakingv1.MatchmakingFailureReason_MATCHMAKING_FAILURE_REASON_RETRIES_EXHAUSTED:
		return "RETRIES_EXHAUSTED"
	default:
		// MATCHMAKING_FAILURE_REASON_UNSPECIFIED, or any future proto value
		// this build doesn't know about yet — surfaced as a distinguishable
		// string rather than silently mapped to RETRIES_EXHAUSTED, so a
		// client or log line can tell the two apart.
		slog.Warn("translateFailureReason: unrecognized proto reason", "reason", reason)
		return "UNKNOWN"
	}
}
