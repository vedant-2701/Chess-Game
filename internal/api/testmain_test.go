//go:build integration

package api

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vedant-2701/chess/internal/store"
)

// testPool is the shared connection pool for all integration tests in this
// package. Initialised once in TestMain and closed after all tests complete.
// Mirrors internal/game/testmain_test.go's pattern exactly.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://chess:chess@localhost:5432/chess_dev?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		os.Stderr.WriteString("api integration tests: failed to connect to database: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		os.Stderr.WriteString("api integration tests: database ping failed: " + err.Error() + "\n")
		os.Stderr.WriteString("  Is the database running? Set TEST_DATABASE_URL or run: make docker-up\n")
		os.Exit(1)
	}

	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// truncateAll removes all rows from every table in dependency order. Called
// at the start of each integration test to guarantee a clean slate.
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
