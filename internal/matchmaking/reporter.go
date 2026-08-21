package matchmaking

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/rpc"
	matchmakingv1 "github.com/vedant-2701/chess/proto/matchmakingv1"
)

// defaultReportMaxAttempts/defaultReportBackoff bound grpcMatchReporter's
// own internal retry loop (PHASE_3_DESIGN_NOTES.md §6's Reliability
// section: "chess-server retries the RPC itself, bounded, with backoff").
// This is a SEPARATE retry loop from PairingLoop.pairAndReport's
// CreateMatchedGame retries — by the time either of grpcMatchReporter's
// methods is called, the game already exists in Postgres (or definitely
// doesn't, for the failure case); these retries only cover the *report*
// possibly failing to reach matchmaking-service, never game creation
// itself.
const defaultReportMaxAttempts = 3
const defaultReportBackoff = 200 * time.Millisecond

// grpcMatchReporter is the concrete, gRPC-backed MatchReporter
// (DECISIONS_LOG_PHASE_3.md ADR-033/ADR-034, PHASE_3_DESIGN_NOTES.md §6) —
// chess-server's client half of MatchReportService. matchmaking-service's
// server-side implementation is a separate piece (Phase 3 Step 4, not this
// package).
type grpcMatchReporter struct {
	client      matchmakingv1.MatchReportServiceClient
	maxAttempts int
	backoff     time.Duration
}

// NewGRPCMatchReporter dials matchmaking-service at target (host:port) and
// returns a MatchReporter backed by the real MatchReportService client,
// plus a close function the caller must invoke during shutdown.
//
// Installs internal/rpc's shared-secret client interceptor
// (DECISIONS_LOG_PHASE_3.md ADR-033's trust boundary) on every outgoing
// call — sharedSecret must be the exact same value matchmaking-service's
// own server-side interceptor checks (Phase 3 Step 4, not this package).
//
// Uses grpc.NewClient (not the deprecated grpc.Dial) with insecure
// transport credentials — mTLS is explicitly out of scope for Phase 3
// (ADR-033: "swap for mTLS post-Phase-3, not solved now"). Insecure
// transport-level credentials here are a separate, narrower thing from the
// shared-secret trust boundary itself, which is what actually authenticates
// the caller at the application layer; TCP-level encryption is simply not
// part of this phase's threat model (Docker Compose, single host).
// grpc.NewClient does not dial eagerly — the connection is established
// lazily on the first RPC, matching this project's general preference for
// constructors that don't perform I/O beyond what's strictly needed to
// construct correctly (store.NewGameStore, game.NewRedisDirectory).
func NewGRPCMatchReporter(target, sharedSecret string) (reporter MatchReporter, closeFn func() error, err error) {
	authInterceptor, err := rpc.NewUnaryClientAuthInterceptor(sharedSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("NewGRPCMatchReporter: %w", err)
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(authInterceptor),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("NewGRPCMatchReporter target=%s: %w", target, err)
	}

	return &grpcMatchReporter{
		client:      matchmakingv1.NewMatchReportServiceClient(conn),
		maxAttempts: defaultReportMaxAttempts,
		backoff:     defaultReportBackoff,
	}, conn.Close, nil
}

// sleepOrDone waits d, or returns ctx.Err() early if ctx is cancelled first
// — CODING_GUIDELINES.md's forbidden-patterns table bans "ignoring
// context.Context cancellation" for exactly this shape of blocking wait; a
// bare time.Sleep here would make a cancelled caller's request hang out the
// full backoff regardless of cancellation.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *grpcMatchReporter) ReportMatchCreated(ctx context.Context, gameID, whiteUserID, blackUserID string, tokens game.MatchedGameTokens, instanceLabel string) error {
	req := &matchmakingv1.MatchCreatedRequest{
		GameId:            gameID,
		WhiteUserId:       whiteUserID,
		BlackUserId:       blackUserID,
		WhitePlayerToken:  tokens.WhitePlayerToken,
		BlackPlayerToken:  tokens.BlackPlayerToken,
		WhiteConnectToken: tokens.WhiteConnectToken,
		BlackConnectToken: tokens.BlackConnectToken,
		InstanceLabel:     instanceLabel,
	}

	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		_, err := r.client.ReportMatchCreated(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Warn("grpcMatchReporter.ReportMatchCreated attempt failed",
			"gameID", gameID, "attempt", attempt, "maxAttempts", r.maxAttempts, "error", err)

		if attempt < r.maxAttempts {
			if sleepErr := sleepOrDone(ctx, r.backoff*time.Duration(attempt)); sleepErr != nil {
				return fmt.Errorf("grpcMatchReporter.ReportMatchCreated gameID=%s: %w", gameID, sleepErr)
			}
		}
	}
	return fmt.Errorf("grpcMatchReporter.ReportMatchCreated gameID=%s: all %d attempts failed: %w",
		gameID, r.maxAttempts, lastErr)
}

func (r *grpcMatchReporter) ReportMatchmakingFailed(ctx context.Context, whiteUserID, blackUserID string) error {
	req := &matchmakingv1.MatchmakingFailedRequest{
		WhiteUserId: whiteUserID,
		BlackUserId: blackUserID,
		Reason:      matchmakingv1.MatchmakingFailureReason_MATCHMAKING_FAILURE_REASON_RETRIES_EXHAUSTED,
	}

	var lastErr error
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		_, err := r.client.ReportMatchmakingFailed(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		slog.Warn("grpcMatchReporter.ReportMatchmakingFailed attempt failed",
			"whiteUserID", whiteUserID, "blackUserID", blackUserID,
			"attempt", attempt, "maxAttempts", r.maxAttempts, "error", err)

		if attempt < r.maxAttempts {
			if sleepErr := sleepOrDone(ctx, r.backoff*time.Duration(attempt)); sleepErr != nil {
				return fmt.Errorf("grpcMatchReporter.ReportMatchmakingFailed whiteUserID=%s blackUserID=%s: %w",
					whiteUserID, blackUserID, sleepErr)
			}
		}
	}
	return fmt.Errorf("grpcMatchReporter.ReportMatchmakingFailed whiteUserID=%s blackUserID=%s: all %d attempts failed: %w",
		whiteUserID, blackUserID, r.maxAttempts, lastErr)
}
