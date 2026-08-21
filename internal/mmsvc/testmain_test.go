//go:build integration

package mmsvc

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// testRedisClient mirrors internal/game/testmain_test.go's and
// internal/matchmaking/testmain_test.go's identical setup — same env var
// (TEST_REDIS_ADDR), same DB 1 isolation from whatever a developer might
// have running against DB 0. This package needs its own copy: Go test
// helpers in _test.go files are package-private and not importable across
// packages.
//
// No testPool here — unlike every other integration-tagged package in this
// module, internal/mmsvc has no Postgres dependency at all
// (ARCHITECTURE.md's Matchmaking section: "matchmaking-service never talks
// to GameStore"), so there is nothing for a shared *pgxpool.Pool to do in
// this package's tests.
var testRedisClient *redis.Client

func TestMain(m *testing.M) {
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	ctx := context.Background()
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 1})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		os.Stderr.WriteString("mmsvc integration tests: redis ping failed: " + err.Error() + "\n")
		os.Stderr.WriteString("  Is redis running? Set TEST_REDIS_ADDR or run: make docker-up\n")
		os.Exit(1)
	}
	testRedisClient = redisClient

	code := m.Run()

	redisClient.Close()
	os.Exit(code)
}

// flushTestRedisDB wipes DB 1 only (see testRedisClient's doc comment) —
// called at the start of every test to guarantee a clean slate. This
// package's own queueKey/activeGameMarkerKey/resultKey/reportedKey live in
// the same DB as internal/game's and internal/matchmaking's test suites use
// (all DB 1) — safe, since these three packages' tests never run
// concurrently against the same Redis instance within a single `go test`
// invocation (each package gets its own test binary).
func flushTestRedisDB(t *testing.T) {
	t.Helper()
	if err := testRedisClient.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushTestRedisDB: %v", err)
	}
}
