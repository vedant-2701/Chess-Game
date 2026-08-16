package mmsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient constructs and verifies a Redis client for
// matchmaking-service.
//
// Deliberately duplicated from internal/game.NewRedisClient's identical
// ping-verify logic, not imported — importing internal/game here would pull
// chess-server's entire game package (and its transitive internal/store,
// internal/chess, internal/ws dependencies) into matchmaking-service's
// binary for the sake of one helper function, exactly the coupling
// DECISIONS_LOG_PHASE_3.md ADR-032 exists to avoid ("matchmaking-service
// never talks to GameStore, GameRegistry, or the Redis ownership keys").
// Both services happen to share the same Redis instance (new key
// namespace, not new infrastructure — ARCHITECTURE.md's Matchmaking
// section), but that is a deployment fact, not a reason for one binary's
// package to import the other's.
//
// redisAddr is a host:port pair, matching docker-compose.yml's redis
// service — not a redis:// URL. A ping failure here is fatal at startup,
// same treatment cmd/server/main.go gives a bad REDIS_ADDR: there is no
// meaningful degraded startup mode for a service that cannot reach the
// queue it exists to serve.
func NewRedisClient(ctx context.Context, redisAddr string) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{Addr: redisAddr})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("mmsvc.NewRedisClient addr=%s: %w", redisAddr, err)
	}

	return client, nil
}
