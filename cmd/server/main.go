// Command server wires every dependency layer together and starts the chess
// server's HTTP/WebSocket listener. See PHASE_1.md Step 13 for the checklist
// this file implements and ARCHITECTURE.md's Dependency Graph for the
// construction order below.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/vedant-2701/chess/internal/api"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// shutdownTimeout bounds every step of graceful shutdown (HTTP drain,
// WebSocket registry drain, clock-state flush). It is deliberately much
// shorter than the 60-second abandonment window — shutdown must not wait
// that out, it only needs to let in-flight requests, in-flight WebSocket
// message handling, and the clock-persist steps complete before the process
// exits.
const shutdownTimeout = 15 * time.Second

// config holds the environment-derived settings PHASE_1.md Step 13 and
// PHASE_2.md Step 1 require.
type config struct {
	DatabaseURL string
	JWTSecret   string
	ServerPort  string
	LogLevel    string
	RedisAddr   string
	InstanceID  string
}

// loadConfig reads and validates required environment variables. DATABASE_URL
// and JWT_SECRET have no safe default and are treated as fatal if missing —
// especially JWT_SECRET, since a default value here would be a real security
// footgun (see .env.example's own warning to change it before running).
// SERVER_PORT and LOG_LEVEL fall back to sensible defaults so a minimal
// environment (DATABASE_URL + JWT_SECRET only) still starts.
func loadConfig() (config, error) {
	// Attempt to load .env, but ignore errors if it doesn't exist
	_ = godotenv.Load()

	cfg := config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		ServerPort:  os.Getenv("SERVER_PORT"),
		LogLevel:    os.Getenv("LOG_LEVEL"),
		RedisAddr:   os.Getenv("REDIS_ADDR"),
		InstanceID:  os.Getenv("INSTANCE_ID"),
	}
	if cfg.DatabaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return config{}, errors.New("JWT_SECRET is required")
	}
	// REDIS_ADDR and INSTANCE_ID are fatal-if-missing, same treatment as
	// DATABASE_URL/JWT_SECRET — not because Phase 1 needs Redis (it never
	// will, per ARCHITECTURE.md's EventBus correction), but because as of
	// Phase 2 wiring this config struct is no longer optional-Redis: a missing
	// INSTANCE_ID in particular must fail loud at startup rather than silently
	// default, since a default shared across multiple replicas would corrupt
	// routing (see .env.example's comment on INSTANCE_ID).
	if cfg.RedisAddr == "" {
		return config{}, errors.New("REDIS_ADDR is required")
	}
	if cfg.InstanceID == "" {
		return config{}, errors.New("INSTANCE_ID is required")
	}
	if cfg.ServerPort == "" {
		cfg.ServerPort = "8080"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return cfg, nil
}

// parseLogLevel maps the LOG_LEVEL string (per .env.example: debug, info,
// warn, error) to a slog.Level. An unrecognized value falls back to Info
// rather than failing startup over a typo in a non-critical setting.
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
		// unrecoverable main.go startup failures. slog isn't configured yet
		// at this point — the log level itself comes from the config we just
		// failed to load — so this is the one place in the codebase where a
		// pre-logging failure is unavoidable rather than a guidelines
		// violation.
		panic(fmt.Sprintf("config: %v", err))
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})))

	ctx := context.Background()

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close() // safety net; the graceful shutdown path also closes this explicitly below.

	if err := runMigrations(cfg.DatabaseURL); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// PHASE_2.md Step 1: verify Redis connectivity at startup. Fatal here,
	// same treatment as the Postgres pool above — there is no meaningful
	// degraded startup mode for an instance that cannot reach its routing
	// directory at all during boot. This is distinct from a Redis outage
	// *after* the server is already serving traffic, which Step 2+'s
	// RoutingDirectory call sites must handle without crashing (already-owned,
	// already-hydrated games keep working; only new resolves fail). The
	// RoutingDirectory itself is not constructed here yet — Step 2.
	redisClient, err := game.NewRedisClient(ctx, cfg.RedisAddr)
	if err != nil {
		slog.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close() // safety net; not yet used by any call path (Step 2+).
	slog.Info("redis connected", "addr", cfg.RedisAddr, "instanceID", cfg.InstanceID)

	// PHASE_2.md Step 5: the routing directory built on top of the verified
	// Redis client above. Constructed here (not deferred to first use) so
	// Manager's constructor always receives a real, non-nil RoutingDirectory
	// in production wiring — nil is only ever passed by tests that don't
	// exercise ResolveGame at all (see NewManager's doc comment).
	directory := game.NewRedisDirectory(redisClient)

	// --- Dependency graph, built bottom-up per ARCHITECTURE.md's Dependency
	// Graph: stores -> validator -> event bus -> move processor -> registry
	// -> manager. Nothing above this line depends on anything constructed
	// below it.
	gameStore := store.NewGameStore(pool)
	moveStore := store.NewMoveStore(pool)
	userStore := store.NewUserStore(pool)

	validator := internalchess.NewValidator()
	eventBus := game.NewLocalEventBus()
	processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
	registry := game.NewGameRegistry()
	manager := game.NewManager(registry, processor, gameStore, moveStore, eventBus, cfg.JWTSecret, validator, directory, cfg.InstanceID)

	if err := manager.RestoreActiveGames(ctx); err != nil {
		slog.Error("failed to restore active games", "error", err)
		os.Exit(1)
	}

	// PHASE_2.md Step 6: start the per-instance heartbeat ticker (renews this
	// instance's liveness key and, once any games exist, their ownership
	// records) now that manager/directory/instanceID all exist. Fatal on
	// failure to start, same treatment as every other Redis-dependent startup
	// step above — SetAlive failing here means this instance cannot even
	// establish its own liveness record, let alone serve traffic correctly.
	stopHeartbeat, err := manager.StartHeartbeat(ctx)
	if err != nil {
		slog.Error("failed to start heartbeat", "error", err)
		os.Exit(1)
	}

	wsRegistry := ws.NewRegistry()

	// wsCtx is the ADR-018 server-lifetime context: created exactly once
	// here, threaded into api.NewRouter (and therefore WSHandler's every
	// onMessage/onClose callback), and cancelled in the SIGTERM branch below
	// — strictly before wsRegistry.CloseAll(), per ADR-018's Consequences.
	wsCtx, cancelWSCtx := context.WithCancel(context.Background())
	defer cancelWSCtx() // safety net if main returns via an unexpected path.

	router := api.NewRouter(manager, userStore, wsRegistry, cfg.JWTSecret, wsCtx)

	httpServer := &http.Server{
		Addr:    ":" + cfg.ServerPort,
		Handler: router,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		slog.Info("server starting", "port", cfg.ServerPort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
			return
		}
		serverErrCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serverErrCh:
		// ListenAndServe only returns a non-nil, non-ErrServerClosed error on
		// immediate startup failure (e.g. port already in use) — it cannot
		// deliver here once the server is actually serving. Nothing has been
		// accepted yet, so there is no graceful-shutdown sequence to run;
		// os.Exit(1) skipping the deferred cleanup above is acceptable.
		if err != nil {
			slog.Error("server failed to start", "error", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())
		shutdown(httpServer, cancelWSCtx, wsRegistry, manager, pool, stopHeartbeat)
	}
}

// shutdown runs PHASE_1.md Step 13's ordered graceful-shutdown sequence,
// extended by PHASE_2.md Step 6's heartbeat release:
//
//  1. Stop accepting new HTTP connections and new WebSocket upgrade attempts.
//  2. Cancel the ADR-018 server-lifetime context, so in-flight
//     Manager.HandleMessage/HandleDisconnect calls observe cancellation on
//     their next context-aware operation.
//  3. Force-close and drain every WebSocket connection. Each connection's
//     onClose callback runs Manager.HandleDisconnect, which — after this
//     session's fix — persists that player's clock state as it disconnects,
//     so this step does most of the real "persist clock state" work as a
//     side effect of draining connections.
//  4. Sweep any game still left in the registry and flush its live clock
//     reading explicitly (Manager.PersistActiveClockState) — defense in
//     depth for anything step 3 missed, and an explicit, auditable
//     realization of PHASE_1.md's "persist clock state" checklist item
//     rather than a purely implicit side effect of connection cleanup.
//  5. Stop the heartbeat ticker and proactively release this instance's
//     Redis ownership/liveness entries (PHASE_2.md Step 6) — placed after
//     step 4 so it reflects the final, post-drain set of games this
//     instance actually still holds, not a stale snapshot from before
//     WebSocket draining/finalizeGame calls removed some of them.
//  6. Close the database pool.
//
// Every step is bounded by shutdownTimeout (or, for step 5's release calls,
// its own independent 5s bound — see releaseHeartbeatEntries) so a single
// stuck connection, slow query, or slow Redis call cannot hang the process
// indefinitely during what is supposed to be a graceful exit.
func shutdown(
	httpServer *http.Server,
	cancelWSCtx context.CancelFunc,
	wsRegistry *ws.Registry,
	manager *game.Manager,
	pool *pgxpool.Pool,
	stopHeartbeat func(),
) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// 1. Stop accepting new HTTP connections and WS upgrade attempts.
	// Shutdown also waits for in-flight *HTTP* handlers to return — but a
	// successfully upgraded WebSocket connection is no longer tracked as an
	// active HTTP request once Upgrade() hijacks it, so this step alone does
	// not wait for WS traffic to drain. Steps 2–3 handle that.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown did not complete cleanly", "error", err)
	}

	// 2. Cancel the server-lifetime context (ADR-018).
	cancelWSCtx()

	// 3. Force-close every WebSocket connection and wait (bounded, internally
	// to Registry.CloseAll) for their read/write/heartbeat goroutines to
	// finish.
	wsRegistry.CloseAll()

	// 4. Defense-in-depth clock flush for anything still registered.
	manager.PersistActiveClockState(shutdownCtx)

	// 5. Stop the heartbeat ticker and release this instance's directory
	// entries (PHASE_2.md Step 6). stopHeartbeat internally detaches its own
	// release calls from shutdownCtx's cancellation (ADR-019/ADR-027 pattern)
	// since shutdownCtx itself will eventually be cancelled by this
	// function's own deferred cancel() — the release must survive that.
	stopHeartbeat()

	// 6. Close the database pool as the final, explicit step.
	pool.Close()

	slog.Info("shutdown sequence complete")
}

// runMigrations applies all pending migrations using golang-migrate's pgx/v5
// driver.
//
// databaseURL arrives in postgres:// form (matching the migrate CLI's own
// usage in the Makefile and .env.example). golang-migrate's pgx/v5 driver
// package (golang-migrate/migrate/v4/database/pgx/v5) registers itself under
// the "pgx5" URL scheme, not "postgres" — verified directly against that
// package's init()/Open() source rather than assumed, since a wrong scheme
// name here fails as an opaque "unknown driver" error rather than a type
// error. Open() internally rewrites the scheme back to postgres:// before
// handing off to database/sql via the pgx/v5 stdlib adapter, which that
// same package already blank-imports — no separate stdlib import is needed
// here.
func runMigrations(databaseURL string) error {
	migrateURL := strings.Replace(databaseURL, "postgres://", "pgx5://", 1)

	// file://migrations is relative to the process's working directory. This
	// mirrors the same assumption already baked into the Makefile's
	// migrate-up target (`migrate -path ./migrations ...`) and the `run`
	// target (`go run ./cmd/server/...`, invoked from the repo root) — both
	// already assume the repo root as cwd. Not a new assumption introduced
	// here.
	m, err := migrate.New("file://migrations", migrateURL)
	if err != nil {
		return fmt.Errorf("runMigrations: create migrate instance: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			slog.Error("migrate source close error", "error", srcErr)
		}
		if dbErr != nil {
			slog.Error("migrate database close error", "error", dbErr)
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("runMigrations: %w", err)
	}
	return nil
}
