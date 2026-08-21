// Command matchmaking-service is Phase 3 Step 4's separate deployable —
// queue intake, SSE notification, and the MatchReportService gRPC receiver
// (DECISIONS_LOG_PHASE_3.md ADR-032). This file wires up all of Step 4's
// HTTP/SSE endpoints, the queue-timeout sweep, and the gRPC server —
// PHASE_3.md's Step 4 checklist is now complete.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"google.golang.org/grpc"

	"github.com/vedant-2701/chess/internal/auth"
	"github.com/vedant-2701/chess/internal/mmsvc"
	"github.com/vedant-2701/chess/internal/rpc"
	matchmakingv1 "github.com/vedant-2701/chess/proto/matchmakingv1"
)

// shutdownTimeout mirrors cmd/server/main.go's identical constant and
// rationale — bounded graceful shutdown, applied to both the HTTP server
// (httpServer.Shutdown) and the gRPC server (a bounded wait around
// GracefulStop, since GracefulStop itself has no built-in timeout).
const shutdownTimeout = 15 * time.Second

// defaultQueueTimeoutSeconds is MATCHMAKING_QUEUE_TIMEOUT_SECONDS' default
// when unset (DECISIONS_LOG_PHASE_3.md ADR-036: "configurable, same
// convention as ADR-032's pairing interval" — no specific default value is
// itself ADR-fixed). 30s chosen as a reasonable middle ground for local
// dev/testing: long enough that a normal pairing-loop tick
// (MATCHMAKING_PAIRING_INTERVAL_MS, default 500ms) has many chances to
// find a match first, short enough to demonstrate the sweep firing in a
// manual test without a long wait. Revisit against
// PHASE_3_DESIGN_NOTES.md's §14 if that document specifies a different
// number more precisely than this session had loaded.
const defaultQueueTimeoutSeconds = 30

// config holds the environment-derived settings this binary needs.
//
// Deliberately does NOT include DATABASE_URL — matchmaking-service never
// talks to Postgres at all (ARCHITECTURE.md's Matchmaking section: "never
// talks to GameStore... only sees outcomes reported to it after the
// fact") — there is no meaningful config field for a dependency this
// service structurally does not have.
type config struct {
	Port      string
	GRPCPort  string
	LogLevel  string
	RedisAddr string

	// JWTSecret must be the exact same value chess-server's own JWT_SECRET
	// holds — MatchmakingClaims/PlayerClaims/ConnectClaims all share one
	// signing secret (internal/auth's existing one-secret convention), not
	// a matchmaking-service-specific key.
	JWTSecret string

	// SharedSecret (MATCHMAKING_SHARED_SECRET) gates the MatchReportService
	// gRPC server (DECISIONS_LOG_PHASE_3.md ADR-033) via
	// internal/rpc.NewUnaryServerAuthInterceptor — must match chess-server's
	// own copy exactly.
	SharedSecret string

	// QueueTimeout is MATCHMAKING_QUEUE_TIMEOUT_SECONDS, parsed into a
	// time.Duration for direct use by mmsvc.NewSweep.
	QueueTimeout time.Duration

	// MatchmakingClaimsTTL is PHASE_3.md Step 5's fix — previously a
	// hardcoded 10s constant in internal/auth (auth.MatchmakingClaimsTTL, now
	// renamed DefaultMatchmakingClaimsTTL and only a fallback default).
	// Optional — falls back to auth.DefaultMatchmakingClaimsTTL if
	// MATCHMAKING_CLAIMS_TTL_SECONDS is unset.
	MatchmakingClaimsTTL time.Duration
}

// loadConfig reads and validates required environment variables.
func loadConfig() (config, error) {
	_ = godotenv.Load()

	cfg := config{
		Port:         os.Getenv("MATCHMAKING_SVC_PORT"),
		GRPCPort:     os.Getenv("MATCHMAKING_SVC_GRPC_PORT"),
		LogLevel:     os.Getenv("LOG_LEVEL"),
		RedisAddr:    os.Getenv("REDIS_ADDR"),
		JWTSecret:    os.Getenv("JWT_SECRET"),
		SharedSecret: os.Getenv("MATCHMAKING_SHARED_SECRET"),
	}
	if cfg.RedisAddr == "" {
		return config{}, errors.New("REDIS_ADDR is required")
	}
	if cfg.JWTSecret == "" {
		return config{}, errors.New("JWT_SECRET is required — must match chess-server's JWT_SECRET exactly (ADR-036's one-secret convention)")
	}
	if cfg.SharedSecret == "" {
		return config{}, errors.New("MATCHMAKING_SHARED_SECRET is required — must match chess-server's copy exactly (ADR-033's trust boundary)")
	}
	if cfg.Port == "" {
		// Matches nginx.conf's matchmaking-service:8081 assumption.
		cfg.Port = "8081"
	}
	if cfg.GRPCPort == "" {
		// Distinct from Port — HTTP/SSE and gRPC are different protocols on
		// different listeners, matching chess-server's own client dial
		// target (MATCHMAKING_SERVICE_ADDR) pointing at this same port in
		// docker-compose.yml.
		cfg.GRPCPort = "9090"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	queueTimeoutSeconds := defaultQueueTimeoutSeconds
	if raw := os.Getenv("MATCHMAKING_QUEUE_TIMEOUT_SECONDS"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 {
			return config{}, fmt.Errorf("MATCHMAKING_QUEUE_TIMEOUT_SECONDS must be a positive integer, got %q", raw)
		}
		queueTimeoutSeconds = parsed
	}
	cfg.QueueTimeout = time.Duration(queueTimeoutSeconds) * time.Second

	// MATCHMAKING_CLAIMS_TTL_SECONDS: optional, PHASE_3.md Step 5. Same
	// safe-default-on-any-parse-failure treatment as the queue timeout above
	// — falls back to auth.DefaultMatchmakingClaimsTTL, not zero.
	claimsTTLSeconds, claimsErr := strconv.Atoi(os.Getenv("MATCHMAKING_CLAIMS_TTL_SECONDS"))
	if claimsErr != nil || claimsTTLSeconds <= 0 {
		cfg.MatchmakingClaimsTTL = auth.DefaultMatchmakingClaimsTTL
	} else {
		cfg.MatchmakingClaimsTTL = time.Duration(claimsTTLSeconds) * time.Second
	}

	// PHASE_3_DESIGN_NOTES.md §18.5 (2026-08-17): MatchmakingClaimsTTL must
	// safely outlive the longest a player could legitimately still be
	// queued — QueueTimeout itself, plus Sweep's fixed 5s tick interval
	// (worst case, a timed-out player waits up to one extra tick before the
	// sweep notices and removes them), plus round-trip margin. Validated
	// here, at startup, rather than left to two independently-configured env
	// vars staying in sync by luck — catches exactly the case where
	// MATCHMAKING_QUEUE_TIMEOUT_SECONDS is raised in some future deployment
	// without MATCHMAKING_CLAIMS_TTL_SECONDS following it up. Fails fast
	// (panics, same treatment every other required-config check in this
	// function already gets) rather than silently letting queued players'
	// tokens expire out from under them mid-wait.
	const minClaimsTTLMargin = 15 * time.Second
	if cfg.MatchmakingClaimsTTL < cfg.QueueTimeout+minClaimsTTLMargin {
		return config{}, fmt.Errorf(
			"MATCHMAKING_CLAIMS_TTL_SECONDS (%s) must be at least MATCHMAKING_QUEUE_TIMEOUT_SECONDS (%s) + %s — "+
				"otherwise a player's own matchmaking token can expire while they are still legitimately queued",
			cfg.MatchmakingClaimsTTL, cfg.QueueTimeout, minClaimsTTLMargin)
	}

	return cfg, nil
}

// parseLogLevel mirrors cmd/server/main.go's identical function exactly.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// panic is CODING_GUIDELINES.md §1's explicit exception for
		// unrecoverable main.go startup failures — same justification as
		// cmd/server/main.go's identical panic (slog isn't configured yet
		// at this point).
		panic(fmt.Sprintf("config: %v", err))
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})))

	ctx := context.Background()

	redisClient, err := mmsvc.NewRedisClient(ctx, cfg.RedisAddr)
	if err != nil {
		slog.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()
	slog.Info("redis connected", "addr", cfg.RedisAddr)

	queue := mmsvc.NewQueue(redisClient)
	hub := mmsvc.NewHub()
	handler := mmsvc.NewHandler(queue, hub, cfg.JWTSecret, cfg.MatchmakingClaimsTTL)
	router := mmsvc.NewRouter(handler)

	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	authInterceptor, err := rpc.NewUnaryServerAuthInterceptor(cfg.SharedSecret)
	if err != nil {
		slog.Error("failed to construct gRPC auth interceptor", "error", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor))
	matchmakingv1.RegisterMatchReportServiceServer(grpcServer, mmsvc.NewReportServer(redisClient, queue, hub))

	grpcListener, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		slog.Error("failed to bind gRPC listener", "port", cfg.GRPCPort, "error", err)
		os.Exit(1)
	}

	sweep := mmsvc.NewSweep(redisClient, queue, hub, cfg.QueueTimeout)
	stopSweep := sweep.Start(ctx)

	serverErrCh := make(chan error, 2)
	go func() {
		slog.Info("matchmaking-service HTTP/SSE starting", "port", cfg.Port)
		if httpErr := httpServer.ListenAndServe(); httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
			serverErrCh <- fmt.Errorf("HTTP server: %w", httpErr)
			return
		}
		serverErrCh <- nil
	}()
	go func() {
		slog.Info("matchmaking-service gRPC starting", "port", cfg.GRPCPort)
		if grpcErr := grpcServer.Serve(grpcListener); grpcErr != nil {
			serverErrCh <- fmt.Errorf("gRPC server: %w", grpcErr)
			return
		}
		serverErrCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serverErrCh:
		if err != nil {
			slog.Error("matchmaking-service failed to start", "error", err)
			stopSweep()
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())

		stopSweep()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if shutdownErr := httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
			slog.Error("HTTP server shutdown did not complete cleanly", "error", shutdownErr)
		}

		// GracefulStop has no built-in timeout, unlike http.Server.Shutdown —
		// bound it manually so a stuck in-flight RPC cannot hang the whole
		// shutdown sequence indefinitely. A forced Stop() after the same
		// shutdownTimeout window mirrors the bounded-wait shape
		// httpServer.Shutdown already gets from shutdownCtx above.
		grpcStopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(grpcStopped)
		}()
		select {
		case <-grpcStopped:
		case <-time.After(shutdownTimeout):
			slog.Warn("gRPC server graceful stop timed out — forcing stop")
			grpcServer.Stop()
		}
	}

	slog.Info("matchmaking-service shutdown complete")
}
