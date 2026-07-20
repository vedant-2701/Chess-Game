//go:build integration

package game

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/vedant-2701/chess/internal/store"
)

// testPool is the shared connection pool for all integration tests in this package.
// Initialised once in TestMain and closed after all tests complete.
var testPool *pgxpool.Pool

// testRedisClient is the shared Redis client for internal/game's Step 2+
// integration tests (RoutingDirectory). Isolated from whatever a developer
// might have running against Redis DB 0 by using DB 1 instead — the Redis
// equivalent of testPool connecting to a separate chess_dev Postgres database
// rather than the default chess database. flushTestRedisDB (called at the
// start of every directory test) only ever touches DB 1.
var testRedisClient *redis.Client

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://chess:chess@localhost:5432/chess_dev?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		os.Stderr.WriteString("game integration tests: failed to connect to database: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		os.Stderr.WriteString("game integration tests: database ping failed: " + err.Error() + "\n")
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
		os.Stderr.WriteString("game integration tests: redis ping failed: " + err.Error() + "\n")
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

// flushTestRedisDB wipes DB 1 only (see testRedisClient's doc comment) — the
// Redis analog of truncateAll, called at the start of every directory test to
// guarantee a clean slate.
func flushTestRedisDB(t *testing.T) {
	t.Helper()
	if err := testRedisClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushTestRedisDB: %v", err)
	}
}

// truncateAll removes all rows from every table in dependency order.
// Called at the start of each integration test to guarantee a clean slate.
func truncateAll(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`TRUNCATE TABLE moves, games, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncateAll: %v", err)
	}
}

// mustCreateUser inserts a user with the given ID and fails the test on error.
func mustCreateUser(t *testing.T, userID string) {
	t.Helper()
	_, err := store.NewUserStore(testPool).CreateOrGetUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("mustCreateUser id=%s: %v", userID, err)
	}
}

// mustCreateActiveGame inserts a game record, sets player_black_id, and advances
// status to ACTIVE. Returns a GameSession hydrated with both player IDs and the
// in-memory state machine already at ACTIVE so moves can be processed immediately.
func mustCreateActiveGame(t *testing.T, gameID, whiteID, blackID string) *GameSession {
	t.Helper()
	ctx := context.Background()
	gs := store.NewGameStore(testPool)

	if err := gs.CreateGame(ctx, &store.Game{
		ID:            gameID,
		PlayerWhiteID: whiteID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   600_000,
		BlackTimeMs:   600_000,
	}); err != nil {
		t.Fatalf("mustCreateActiveGame CreateGame: %v", err)
	}
	if err := gs.UpdatePlayerBlack(ctx, gameID, blackID); err != nil {
		t.Fatalf("mustCreateActiveGame UpdatePlayerBlack: %v", err)
	}
	// fromStatus is WAITING: this helper builds a freshly-created game up to
	// ACTIVE, mirroring the only real edge into ACTIVE (session.go's
	// validTransitions has no other one).
	if err := gs.UpdateGameStatus(ctx, gameID, store.GameStatusWaiting, store.GameStatusActive, nil); err != nil {
		t.Fatalf("mustCreateActiveGame UpdateGameStatus: %v", err)
	}

	session := NewGameSession(gameID, whiteID)
	session.SetPlayerBlack(blackID)
	if err := session.Transition(store.GameStatusActive); err != nil {
		t.Fatalf("mustCreateActiveGame Transition: %v", err)
	}
	return session
}
