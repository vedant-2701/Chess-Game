//go:build integration

package matchmaking

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
)

// testPool and testRedisClient mirror internal/game/testmain_test.go's setup
// exactly — same env vars (TEST_DATABASE_URL/TEST_REDIS_ADDR), same Redis
// DB 1 isolation. This package needs its own copy: Go test helpers in
// _test.go files are package-private and not importable across packages,
// even between two internal/ packages in the same module.
var testPool *pgxpool.Pool
var testRedisClient *redis.Client

const testJWTSecret = "test-secret-for-matchmaking-package"

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://chess:chess@localhost:5432/chess_dev?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		os.Stderr.WriteString("matchmaking integration tests: failed to connect to database: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		os.Stderr.WriteString("matchmaking integration tests: database ping failed: " + err.Error() + "\n")
		os.Stderr.WriteString("  Is the database running? Set TEST_DATABASE_URL or run: make docker-up\n")
		os.Exit(1)
	}
	testPool = pool

	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 1})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		os.Stderr.WriteString("matchmaking integration tests: redis ping failed: " + err.Error() + "\n")
		os.Stderr.WriteString("  Is redis running? Set TEST_REDIS_ADDR or run: make docker-up\n")
		pool.Close()
		os.Exit(1)
	}
	testRedisClient = redisClient

	code := m.Run()

	pool.Close()
	redisClient.Close()
	os.Exit(code)
}

// truncateAll removes all rows from every table in dependency order.
func truncateAll(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`TRUNCATE TABLE moves, games, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncateAll: %v", err)
	}
}

// flushTestRedisDB wipes DB 1 only (see testRedisClient's doc comment
// above) — this package's queueKey lives in the same DB, so a stale member
// from a previous test would otherwise silently corrupt the next test's
// queue-count assertions.
func flushTestRedisDB(t *testing.T) {
	t.Helper()
	if err := testRedisClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushTestRedisDB: %v", err)
	}
}

// mustCreateUser inserts a user with the given ID and fails the test on
// error — CreateMatchedGame's INSERT has a foreign-key constraint on both
// player_white_id and player_black_id (migrations/002_create_games.up.sql),
// so any test pairing real users must call this first.
func mustCreateUser(t *testing.T, userID string) {
	t.Helper()
	_, err := store.NewUserStore(testPool).CreateOrGetUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("mustCreateUser id=%s: %v", userID, err)
	}
}

// newTestManager builds a fully-wired *game.Manager against testPool/
// testRedisClient, mirroring cmd/server/main.go's dependency-graph
// construction order exactly. Each call gets its own RedisDirectory over the
// same shared testRedisClient (directory.go's ownership keys are gameID-
// keyed, not connection-keyed, so multiple Manager instances safely share
// one underlying Redis connection — exactly how a real multi-instance
// docker-compose deployment works, and precisely what
// TestPairingLoop_ConcurrentTicks_NoDoubleMatch needs to simulate two
// separate chess-server instances).
func newTestManager(t *testing.T, instanceID string) *game.Manager {
	t.Helper()
	directory := game.NewRedisDirectory(testRedisClient)
	validator := internalchess.NewValidator()
	eventBus := game.NewLocalEventBus()
	gameStore := store.NewGameStore(testPool)
	moveStore := store.NewMoveStore(testPool)
	processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
	registry := game.NewGameRegistry()
	return game.NewManager(registry, processor, gameStore, moveStore, eventBus, testJWTSecret, validator, directory, instanceID)
}
