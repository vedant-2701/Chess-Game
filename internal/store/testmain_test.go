//go:build integration

package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool is the shared connection pool for all integration tests in this package.
// Initialised once in TestMain and closed after all tests complete.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		// Fall back to the docker-compose defaults so `make test-integration`
		// works out of the box without setting the variable manually.
		dbURL = "postgres://chess:chess@localhost:5432/chess_dev?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		// Use Fprintf + os.Exit rather than log.Fatal to avoid importing log.
		// TestMain runs before the test binary initialises its own logger.
		os.Stderr.WriteString("store integration tests: failed to connect to database: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		os.Stderr.WriteString("store integration tests: database ping failed: " + err.Error() + "\n")
		os.Stderr.WriteString("  Is the database running? Set TEST_DATABASE_URL or run: make docker-up\n")
		os.Exit(1)
	}

	testPool = pool

	code := m.Run()

	pool.Close()
	os.Exit(code)
}

// truncateAll removes all rows from every table in dependency order.
// Called at the start of each test to guarantee a clean slate.
// Using TRUNCATE ... CASCADE handles FK dependencies automatically.
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
	us := newUserStore()
	_, err := us.CreateOrGetUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("mustCreateUser id=%s: %v", userID, err)
	}
}

// mustCreateGame inserts a game with the given ID and white player ID.
// Applies sensible defaults for FEN and clock values.
func mustCreateGame(t *testing.T, gameID, whitePlayerID string) {
	t.Helper()
	gs := newGameStore()
	err := gs.CreateGame(context.Background(), &Game{
		ID:            gameID,
		PlayerWhiteID: whitePlayerID,
		CurrentFEN:    StartingFEN,
		WhiteTimeMs:   600_000,
		BlackTimeMs:   600_000,
	})
	if err != nil {
		t.Fatalf("mustCreateGame gameID=%s: %v", gameID, err)
	}
}

// Convenience constructors so test files don't need to repeat testPool.
func newUserStore() *UserStore { return NewUserStore(testPool) }
func newGameStore() *GameStore { return NewGameStore(testPool) }
func newMoveStore() *MoveStore { return NewMoveStore(testPool) }
