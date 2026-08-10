> **SUPERSEDED — DO NOT IMPLEMENT.** This is the original Redis-pub/sub cross-instance
> design, kept only for history. It was audited against the actual codebase and found
> to have several critical bugs (see `PHASE_2_FLOW_WALKTHROUGH.md`'s audit notes and
> `DECISIONS_LOG_PHASE_2.md` ADR-021 for the full reasoning). The current Phase 2 design
> — co-located sessions with a Redis-backed ownership/liveness routing directory — lives
> in `phases/current/PHASE_2.md`. Read that file, not this one, for anything current.

# Phase 2 — Horizontal Scaling (SUPERSEDED — see PHASE_2.md)

**Status: ⬜ Not Started**
**Prerequisite: Phase 1 all acceptance criteria met**

---

## Objective

Solve the WebSocket horizontal scaling problem. Run two instances of the chess server behind a load balancer. Players connected to different instances must be able to play a complete game with no degradation in behavior.

**The single most important learning outcome of Phase 2:**
Understanding *why* stateful WebSocket servers cannot be naively horizontally scaled, and why Redis pub/sub is the correct solution at this scale.

---

## The Problem This Phase Solves

In Phase 1, all state lives in one process. Player A and Player B are both connected to Server Instance 1. Everything works.

Now run two instances:

```
Player A ──► Server Instance 1
Player B ──► Server Instance 2
```

Player A sends a move. Server 1 validates it, persists it, and tries to broadcast `MOVE_APPLIED` to Player B. But Player B's `*ws.Connection` is on Server 2. Server 1 has no pointer to it. The move is persisted to the database correctly but Player B never receives the event.

**The game is silently broken.** This is the exact failure mode that Redis pub/sub solves.

---

## Scope

### In Scope

- Redis pub/sub as cross-instance event bus
- `RedisEventBus` implementation of the existing `EventBus` interface
- Nginx or Caddy load balancer configuration for WebSocket connections
- Two server instances running locally via docker-compose
- Graceful handling of Redis connection failure
- Deployment configuration: docker-compose with two server replicas + load balancer

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| Redis persistence (AOF/RDB) | Pub/sub is fire-and-forget; durability is PostgreSQL's job |
| Redis Cluster | Single Redis instance is sufficient for this scale |
| Sticky sessions | This is the anti-pattern being replaced — must not be used |
| Redis as a cache | Not needed yet |
| Kubernetes | Operational complexity with no learning benefit at this stage |
| Multiple Redis instances / Redis Sentinel | Over-engineering for a learning project |

---

## Prerequisites from Phase 1

Before Phase 2 begins, verify:

- [ ] `EventBus` interface exists in `internal/game/eventbus.go`
- [ ] `LocalEventBus` is the current implementation
- [ ] `LocalEventBus` is injected via `main.go`, not hardcoded in game logic
- [ ] All game events go through the EventBus, not via direct `*ws.Connection` writes
- [ ] PostgreSQL is the source of truth for all game state
- [ ] Server restart recovery works (Phase 1 acceptance criterion 3)

If the EventBus interface was not properly built in Phase 1, fix it before writing any Phase 2 code. Phase 2's entire design depends on this seam.

---

## Architecture Change

### Phase 1 Architecture (Single Instance)
```
Player A ──WS──► Server ──► LocalEventBus ──► Player B (same process)
```

### Phase 2 Architecture (Multi-Instance)
```
Player A ──WS──► Server 1 ──► RedisEventBus ──► Redis ──► Server 2 ──WS──► Player B
                                    │                          │
                              Publish to                  Subscribe to
                            game:{gameID}               game:{gameID}
```

Every server instance subscribes to game channels it is currently hosting. When a move arrives:
1. Server 1 receives the move from Player A
2. Validates, persists to PostgreSQL (unchanged from Phase 1)
3. Publishes `MOVE_APPLIED` event to Redis channel `game:{gameID}`
4. Redis delivers the event to all subscribers — including Server 2
5. Server 2 receives the event, looks up Player B's local `*ws.Connection`, and sends the message

---

## Key Technical Challenges

### Challenge 1: Redis pub/sub is fire-and-forget

Redis pub/sub has **at-most-once delivery**. If a subscriber is not connected when a message is published, the message is lost.

**Implication:** If Server 2 restarts between the publish and the subscribe, the message is lost. The client will not receive `MOVE_APPLIED`. The client must handle this by requesting a full state sync (`GAME_STATE`) if it detects a gap.

**What this teaches:** The difference between at-most-once (pub/sub), at-least-once (persistent queues), and exactly-once delivery. Understanding which guarantee is appropriate for each use case.

### Challenge 2: Server restart during an active game

When Server 2 restarts:
1. Player B's WebSocket connection drops
2. Player B reconnects (reconnection logic from Phase 1 handles this)
3. Player B may reconnect to Server 1 or Server 2 depending on load balancer
4. The new server instance must subscribe to `game:{gameID}` when Player B reconnects
5. `RestoreActiveGames` from Phase 1 must also restore Redis subscriptions

### Challenge 3: Load balancer WebSocket configuration

WebSocket upgrade requires specific load balancer configuration:
- `Upgrade` and `Connection` headers must be proxied, not consumed
- Timeout must be significantly longer than HTTP (WebSockets are long-lived)
- Health checks must target `/health`, not `/`

Nginx requires explicit WebSocket proxy configuration. This is not automatic.

### Challenge 4: What happens when Redis goes down

Redis pub/sub going down means inter-instance communication fails. Players on different instances stop receiving moves from each other.

**Phase 2 approach:** Detect Redis unavailability, log it, and degrade gracefully. Games where both players happen to be on the same instance continue working. Games across instances fail silently from the player's perspective — which is a bad experience but acceptable for a learning project at this stage.

**What this teaches:** Thinking about failure modes, not just the happy path.

---

## New Components

### `internal/game/redis_eventbus.go`

Drop-in replacement for `LocalEventBus`. Implements the same `EventBus` interface.

```go
type RedisEventBus struct {
    client *redis.Client
    // each gameID maps to its subscription goroutine
    subscriptions map[string]func()  // gameID -> cancel func
    mu            sync.Mutex
}

func (b *RedisEventBus) Publish(ctx context.Context, event GameEvent) error {
    payload, err := json.Marshal(event)
    if err != nil {
        return fmt.Errorf("RedisEventBus.Publish marshal: %w", err)
    }
    return b.client.Publish(ctx, "game:"+event.GameID, payload).Err()
}

func (b *RedisEventBus) Subscribe(ctx context.Context, gameID string) (<-chan GameEvent, func(), error) {
    // Subscribe to Redis channel, return a Go channel and an unsubscribe function
}
```

**Startup injection (main.go):**
```go
// Phase 1:
// eventBus := game.NewLocalEventBus()

// Phase 2:
redisClient := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_URL")})
eventBus := game.NewRedisEventBus(redisClient)
```

Zero changes to game logic. Only `main.go` changes.

### Updated `docker-compose.yml`

```yaml
services:
  postgres:
    image: postgres:16-alpine
    # ... unchanged

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"

  server-1:
    build: .
    environment:
      - SERVER_PORT=8080
      - DATABASE_URL=...
      - REDIS_URL=redis:6379
    ports:
      - "8080:8080"

  server-2:
    build: .
    environment:
      - SERVER_PORT=8081
      - DATABASE_URL=...
      - REDIS_URL=redis:6379
    ports:
      - "8081:8081"

  nginx:
    image: nginx:alpine
    ports:
      - "80:80"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf
```

---

## Implementation Checklist

### Step 1: Redis Infrastructure
- [ ] Uncomment Redis service in docker-compose.yml
- [ ] Add `go-redis/v9` dependency
- [ ] Implement Redis client initialization in main.go
- [ ] Verify Redis connection on startup, fail fast if unavailable
- [ ] Add REDIS_URL to .env.example

### Step 2: RedisEventBus Implementation
- [ ] `internal/game/redis_eventbus.go`: full implementation
- [ ] `Publish`: serialize GameEvent to JSON, publish to `game:{gameID}` channel
- [ ] `Subscribe`: create Redis subscription, forward messages to Go channel, return unsubscribe func
- [ ] Goroutine management: subscription goroutines must be tracked and cleaned up on game completion
- [ ] Error handling: Redis publish failure must not crash the server
- [ ] Unit tests: publish + subscribe delivers message (requires running Redis — integration tag)

### Step 3: EventBus Swap in main.go
- [ ] Replace `LocalEventBus` with `RedisEventBus` in main.go
- [ ] Run existing Phase 1 tests against RedisEventBus — all must still pass

### Step 4: Load Balancer Configuration
- [ ] Create `nginx.conf` with WebSocket proxy configuration
- [ ] WebSocket-specific headers: `Upgrade`, `Connection`
- [ ] Upstream configuration with both server instances
- [ ] Long timeout configuration (3600s for WebSocket)
- [ ] Health check endpoint: `/health`
- [ ] Test: both server instances receive traffic

### Step 5: Multi-Instance Testing
- [ ] `make docker-up` starts all services: postgres, redis, server-1, server-2, nginx
- [ ] Manually verify Player A connects to server-1 and Player B connects to server-2
- [ ] (Check logs to confirm different instances)
- [ ] Play a complete game — verify all moves arrive correctly on both sides
- [ ] Kill server-2 mid-game, verify Player B can reconnect to server-1 and resume

### Step 6: Failure Mode Testing
- [ ] Kill Redis mid-game
- [ ] Verify server does not crash
- [ ] Verify error is logged with gameID
- [ ] Verify players receive appropriate disconnection/error messages
- [ ] Restart Redis — verify new games work (existing games may need reconnection)

### Step 7: Update Documentation
- [ ] Update ARCHITECTURE.md: new architecture diagram, RedisEventBus section
- [ ] Add ADR-XXX: Redis pub/sub chosen over sticky sessions
- [ ] Add ADR-XXX: At-most-once delivery accepted for Phase 2 (vs at-least-once)
- [ ] Update CLAUDE.md

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Two players connected to different server instances complete a full game |
| 2 | All Phase 1 acceptance criteria still pass with the multi-instance setup |
| 3 | Killing one server instance mid-game: player reconnects to remaining instance, game resumes |
| 4 | Redis going down does not crash either server instance |
| 5 | Load balancer correctly proxies WebSocket upgrade (Connection and Upgrade headers) |
| 6 | All tests pass: `go test -race ./...` |
| 7 | Server logs show correct gameID and instance identification on every event |

---

## Technical Debt Carried From Phase 1

Review CLAUDE.md for TD items that must be resolved before or during Phase 2.

## Technical Debt This Phase May Introduce

| ID | Description | Must Fix By |
|----|-------------|-------------|
| TD-P2-001 | At-most-once Redis delivery — missed messages cause client desync | Phase 5 or when spectators need reliable delivery |
| TD-P2-002 | No Redis health check or circuit breaker | Phase 6 |
| TD-P2-003 | Subscription goroutine leak if unsubscribe not called on game completion | Verify in Phase 2 tests |
