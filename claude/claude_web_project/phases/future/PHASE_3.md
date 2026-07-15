# Phase 3 — Matchmaking

**Status: ⬜ Not Started**
**Prerequisite: Phase 2 all acceptance criteria met**

---

## Objective

Build a matchmaking system where players can enter a queue and be automatically paired into a game. The queue must be safe across multiple server instances — two servers must never match the same player twice.

**The single most important learning outcome of Phase 3:**
Distributed race conditions are real, silent, and dangerous. Atomic operations are the correct solution. Locks are the wrong solution at this scale.

---

## The Problem This Phase Solves

Phase 1 and 2 require players to share a link. That is not a real chess experience. Phase 3 adds: player enters the queue, waits, gets paired with another player, and a game starts automatically.

The naive implementation works fine on a single server. It breaks silently under multiple instances.

**The race condition:**

```
Server 1 reads queue: [PlayerA, PlayerB]  →  decides to match PlayerA + PlayerB
Server 2 reads queue: [PlayerA, PlayerB]  →  also decides to match PlayerA + PlayerB

Result: PlayerA is in two games simultaneously. PlayerB is in two games.
Both games have wrong state. Both players are confused.
```

This is not a hypothetical. It happens every time two workers race on the same queue.

---

## Scope

### In Scope

- Single queue: 10+0 time control only (expanding time controls is Phase 4)
- Redis sorted set as the queue data structure
- Atomic dequeue using Redis ZPOPMIN (prevents race conditions without distributed locks)
- Matchmaking as a goroutine within the existing server (not a separate service — that consideration is Phase 3's design discussion)
- Players enter queue via HTTP POST, exit queue via HTTP DELETE or WebSocket disconnect
- Matched players notified via WebSocket
- Abandoned queue entries (player disconnected) cleaned up automatically
- ELO is not available yet — queue is FIFO by wait time

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| ELO-based matching | Phase 4: ELO doesn't exist yet |
| Multiple time controls in queue | Complexity without learning benefit at this stage |
| Separate matchmaking microservice | Design question to evaluate — may stay in monolith |
| Geographic matching | Not a learning objective |
| Re-queue after no match found | Implement wait-and-expand instead |

---

## Architecture Decision Required Before Phase 3

**Should matchmaking be a separate service or a component within the existing server?**

This is a genuine design question to evaluate at phase start, not a foregone conclusion.

**Arguments for keeping it in the server:**
- No network call overhead for game creation after match
- Simpler deployment
- Matchmaking state (who is queued) is already in Redis — accessible from any instance

**Arguments for extracting to a separate service:**
- Matchmaking logic scales differently from game serving
- Dedicated service can be upgraded independently
- Conceptually distinct responsibility

**Recommendation to evaluate at phase start:** Start as a component within the server. If the queue logic becomes complex enough to justify separation, extract it. Do not extract prematurely.

Document this decision as a new ADR at phase start.

---

## Key Technical Challenges

### Challenge 1: ZPOPMIN vs Read-Then-Delete

**Wrong approach:**
```
Server 1: ZRANGE queue 0 1  →  gets [PlayerA, PlayerB]
Server 2: ZRANGE queue 0 1  →  gets [PlayerA, PlayerB]
Server 1: ZREM queue PlayerA PlayerB
Server 2: ZREM queue PlayerA PlayerB  ←  race condition: both matched the same pair
```

**Correct approach:**
```
ZPOPMIN queue 2  ←  atomic: removes AND returns two lowest-score members in one operation
```

`ZPOPMIN` is atomic. Two servers cannot pop the same pair. One will get the pair. The other will get an empty result or a different pair.

**What this teaches:** Atomic operations eliminate entire classes of race conditions. This is why Redis sorted sets are used for queues — not because they are the only option, but because their operations are atomic and the data structure fits the use case.

### Challenge 2: Odd Queue Numbers

What happens when 3 players are in the queue and the matchmaker runs?

```
Queue: [PlayerA, PlayerB, PlayerC]
ZPOPMIN queue 2  →  matches PlayerA + PlayerB
PlayerC remains in queue, waiting for a fourth player
```

This is correct behavior. PlayerC waits. The matchmaker runs periodically (every 1-2 seconds) and will match PlayerC when another player arrives.

### Challenge 3: Abandoned Queue Entries

Player enters the queue, then closes the browser. Their WebSocket disconnects. But they are still in the Redis queue. If another player is matched with an abandoned entry, the game starts — but one player is already gone.

**Solution:** Queue entries have a TTL (time-to-live). When a player disconnects, their queue entry is immediately removed (`ZREM`). TTL is a fallback for crash scenarios where the ZREM did not execute.

### Challenge 4: Player Already in Queue or Game

A player should not be able to enter the queue if they are already in the queue or in an active game. This requires checking state before enqueue. The check-then-act is not atomic — another layer of defense (TTL + idempotent game creation) is needed.

---

## Queue Data Structure

Redis sorted set: `matchmaking:queue:10+0`

- Member: `userID`
- Score: Unix timestamp of when the player entered the queue (lower score = waited longer)

```
ZADD matchmaking:queue:10+0 1700000000 "user-abc123"  ←  enqueue
ZPOPMIN matchmaking:queue:10+0 2                       ←  atomic dequeue of 2
ZREM matchmaking:queue:10+0 "user-abc123"              ←  leave queue
ZSCORE matchmaking:queue:10+0 "user-abc123"            ←  check if in queue
ZCARD matchmaking:queue:10+0                           ←  queue depth metric
```

---

## New Endpoints

```
POST /matchmaking/queue
  Body: { "userID": "..." }
  Response 200: { "status": "queued", "position": 3 }
  Response 409: { "error": "already in queue" }
  Response 409: { "error": "already in active game" }

DELETE /matchmaking/queue
  Body: { "userID": "..." }
  Response 200: { "status": "dequeued" }

GET /matchmaking/queue/status
  Query: ?userID=...
  Response: { "status": "queued" | "matched" | "not_queued", "gameID": "..." if matched }
```

---

## New WebSocket Messages

When a match is found, both players receive:

```json
{
  "type": "MATCH_FOUND",
  "gameID": "uuid",
  "color": "WHITE",
  "playerToken": "jwt",
  "opponentID": "user-xyz"
}
```

Players who were waiting in the queue should maintain a WebSocket connection to the matchmaking endpoint to receive this notification without polling.

**Note (added post-Phase-2 redesign):** `MATCH_FOUND`'s `playerToken` is the same
long-lived `PlayerClaims` type Phase 1/2 already use — it is *not* sufficient on
its own to open the game's WebSocket connection under the Phase 2 routing design.
After receiving `MATCH_FOUND`, the client follows the same `resolve → connect`
flow used everywhere else (`GET /games/:id/resolve` with `playerToken`, then
connect to the returned masked URL with the short-lived `ConnectClaims` it
returns) — see `phases/current/PHASE_2.md`'s Connection Flow section. This is
deliberate: match-found-then-connect and ordinary-reconnect should be the same
code path, not two.

**Design decision to make:** Should matchmaking use the same WebSocket connection as gameplay, or a separate one? Evaluate at phase start.

---

## Matchmaking Loop

A matchmaking goroutine runs on each server instance. It runs every 1 second.

```
Every 1 second:
  count = ZCARD matchmaking:queue:10+0
  if count < 2: continue
  
  pair = ZPOPMIN matchmaking:queue:10+0 2
  if len(pair) < 2: continue  ←  another instance got there first
  
  playerWhiteID = pair[0]
  playerBlackID = pair[1]
  
  game = CreateGame(playerWhiteID, playerBlackID)
  
  notify playerWhite: MATCH_FOUND
  notify playerBlack: MATCH_FOUND
```

**Important:** Each server instance runs this loop. `ZPOPMIN` being atomic means only one instance will successfully dequeue each pair. If the queue has 2 players and 3 instances run simultaneously, only 1 instance will get the pair. The others get empty results.

---

## Implementation Checklist

### Step 1: Queue Data Layer
- [ ] `internal/matchmaking/queue.go`:
  - [ ] `Queue` struct (wraps Redis client)
  - [ ] `Enqueue(ctx, userID string, timeControl string) error`
  - [ ] `Dequeue(ctx, timeControl string, count int) ([]string, error)` — uses ZPOPMIN
  - [ ] `Remove(ctx, userID string, timeControl string) error`
  - [ ] `IsQueued(ctx, userID string, timeControl string) (bool, error)`
  - [ ] `Depth(ctx, timeControl string) (int64, error)` — for metrics
- [ ] Integration tests (Redis required):
  - [ ] Enqueue adds entry
  - [ ] ZPOPMIN is atomic: simulate concurrent dequeue, verify no double-match
  - [ ] Remove cleans up entry
  - [ ] Depth returns correct count

### Step 2: Matchmaking Service
- [ ] `internal/matchmaking/service.go`:
  - [ ] `Service` struct (depends on Queue, game.Manager)
  - [ ] `Start(ctx context.Context)` — starts matchmaking loop goroutine
  - [ ] `Stop()` — cleanly shuts down loop
  - [ ] `EnqueuePlayer(ctx, userID string) error` — validates not already queued/in-game
  - [ ] `DequeuePlayer(ctx, userID string) error`
  - [ ] Match loop: ZPOPMIN, CreateGame, NotifyBothPlayers
  - [ ] Abandoned entry cleanup: check player still connected before notifying
- [ ] Unit tests for service logic (mock queue)

### Step 3: HTTP Endpoints
- [ ] `internal/api/matchmaking_handler.go`:
  - [ ] POST /matchmaking/queue
  - [ ] DELETE /matchmaking/queue
  - [ ] GET /matchmaking/queue/status
- [ ] Register routes in chi router
- [ ] Handler tests (httptest)

### Step 4: Player Notification
- [ ] Decide: WebSocket or polling for match notification
  - [ ] Document decision as ADR
  - [ ] Implement chosen approach
- [ ] `MATCH_FOUND` message delivered to both players with gameID and playerToken
- [ ] Players can immediately open WebSocket game connection with provided token

### Step 5: Disconnect Cleanup
- [ ] On player WebSocket disconnect: check if player is in matchmaking queue
- [ ] If queued: `ZREM` from queue immediately
- [ ] Add TTL to queue entries as fallback: entries older than 2 minutes auto-expire

### Step 6: Integration Testing
- [ ] Two players enter queue → both receive MATCH_FOUND → game starts
- [ ] Three players enter queue → first two matched, third waits
- [ ] Player enters queue then disconnects → queue entry removed, not matched with anyone
- [ ] Simulate two server instances running matchmaking loops simultaneously → no player double-matched

### Step 7: Documentation
- [ ] Update ARCHITECTURE.md: matchmaking component, queue data flow
- [ ] Add ADR for matchmaking architecture (monolith component vs separate service)
- [ ] Add ADR for atomic ZPOPMIN vs distributed lock approach
- [ ] Update CLAUDE.md

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Two players entering the queue are matched and receive MATCH_FOUND |
| 2 | A player cannot be matched with themselves |
| 3 | A player who disconnects while queued is removed from the queue |
| 4 | Simulate two concurrent matchmaking loops: no player is ever matched twice |
| 5 | Queue depth is correctly reported and measurable |
| 6 | All Phase 1 and 2 acceptance criteria still pass |
| 7 | `go test -race ./...` passes |

---

## Technical Debt This Phase May Introduce

| ID | Description | Must Fix By |
|----|-------------|-------------|
| TD-P3-001 | FIFO queue only — no ELO-based pairing | Phase 4 (ELO exists then) |
| TD-P3-002 | Single time control (10+0) only | Phase 4 |
| TD-P3-003 | Match notification mechanism may need revisit for spectator feature | Phase 5 |
