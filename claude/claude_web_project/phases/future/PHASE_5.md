# Phase 5 — Spectators

**Status: ⬜ Not Started**
**Prerequisite: Phase 4 all acceptance criteria met**

---

## Objective

Allow any number of users to watch a live game in real time. Spectators receive every move as it happens, can join mid-game and receive current board state, and cannot interact with the game.

**The single most important learning outcome of Phase 5:**
The fan-out write problem. One write (a move) must produce N reads (push to N spectators). Synchronous fan-out blocks the move pipeline. Async fan-out introduces ordering guarantees you must think about.

---

## The Problem This Phase Solves

A move arrives. Currently, the server broadcasts to exactly 2 recipients: White and Black. With spectators, the same move must go to N+2 recipients where N could be 0 or 1000.

If broadcasting to spectators is synchronous — iterating over all spectator connections and calling `Send()` on each before returning — then a slow or unresponsive spectator connection can delay the move pipeline. A player should never wait for spectator delivery.

---

## Scope

### In Scope

- Any authenticated or anonymous user can watch any active game
- Spectator WebSocket endpoint: `GET /ws/game/:id/spectate`
- No token required for spectating (games are public)
- Spectator joining mid-game receives current board state immediately
- Spectator disconnecting has no effect on the game
- Move broadcasts to spectators are async (do not block move pipeline)
- Spectator count visible to players (optional, low priority)

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| Spectator chat | Not a system design concept |
| Private games (spectators blocked) | Phase 6+ |
| Spectator ELO requirement | Not relevant |
| Move annotations for spectators | Not relevant |
| Spectator count analytics | Phase 6 (observability) |

---

## Key Technical Challenges

### Challenge 1: Synchronous vs Async Fan-out

**Synchronous (wrong):**
```go
// In move pipeline, after persisting:
for _, spectator := range session.Spectators() {
    spectator.Send(moveAppliedMsg)  // blocks if spectator is slow
}
```
One slow spectator WebSocket blocks move delivery to all other spectators and delays the next move.

**Async (correct):**
```go
// Publish to spectator channel — non-blocking
session.SpectatorBus.Publish(moveAppliedMsg)

// Separate goroutine per spectator consumes from the channel
```

Move pipeline does not wait for spectator delivery. Spectator delivery failures do not affect gameplay.

### Challenge 2: Mid-Game Join

When a spectator joins mid-game, they need the current board state. This is a read from the `GameSession` or from PostgreSQL.

**Race condition to think about:** Spectator joins while a move is being processed. The move is persisted but not yet broadcast. The spectator gets the pre-move FEN. Then they receive the `MOVE_APPLIED` event. Is this correct?

Yes, if `MOVE_APPLIED` events are ordered and the spectator processes them in order. The spectator receives the FEN at join time, then applies subsequent moves in order. This is correct.

### Challenge 3: Spectator Connection Cleanup

Spectators disconnect constantly (tab close, network drop). A disconnected spectator's goroutine must terminate cleanly. If spectators are stored in a slice and never removed, the slice grows without bound and broadcast iterates over dead connections indefinitely.

Use the same `ws.Registry` pattern from Phase 1: a map with registration/unregistration. On spectator disconnect, unregister. Fan-out iterates only registered spectators.

### Challenge 4: Cross-Instance Spectators (Phase 2 Integration)

A spectator may connect to a different server instance than the players. The existing `RedisEventBus` already solves this: spectator events use the same `game:{gameID}` channel. The spectator's instance subscribes to the channel and delivers to the spectator's connection.

No new infrastructure needed. This is a validation that the Phase 2 architecture was designed correctly.

---

## Architecture Change

### Spectator Registry

A `SpectatorRegistry` is added to `GameSession`, separate from the two player connection slots.

```go
type GameSession struct {
    // ... existing fields ...

    // Spectators: map of connID -> *ws.Connection
    // Protected by session mutex
    spectators map[string]*ws.Connection
}
```

### Updated Move Pipeline

```
Move persisted → MOVE_APPLIED event published to EventBus
                                        │
                        ┌───────────────┴─────────────────┐
                        │                                  │
                 Player delivery                  Spectator fan-out
                 (existing, sync)                 (new, async goroutine)
                        │                                  │
                 White gets msg                   All spectators get msg
                 Black gets msg                   independently
```

---

## New Endpoints and Messages

```
GET /ws/game/:id/spectate
  No token required
  On connect: receive GAME_STATE (current board)
  On move: receive MOVE_APPLIED (same format as players)
  On game end: receive GAME_OVER

GET /games/:id/spectators
  Response: { "count": 42 }  (optional, low priority)
```

---

## Implementation Checklist

### Step 1: Spectator Connection Handling
- [ ] `internal/ws/handler.go`: add spectate endpoint handling
- [ ] On spectate connect: no token validation required (public games)
- [ ] Register spectator into `GameSession.spectators` map
- [ ] Send current `GAME_STATE` to spectator on join
- [ ] On disconnect: unregister from `GameSession.spectators`

### Step 2: Async Fan-out
- [ ] `internal/game/session.go`: add `spectators map[string]*ws.Connection`
- [ ] `BroadcastToSpectators(msg []byte)` — non-blocking, runs in separate goroutine
- [ ] Move pipeline: after player delivery, call `BroadcastToSpectators` asynchronously
- [ ] Spectator send failure: log and remove from registry (do not crash, do not block)

### Step 3: Mid-Game State Delivery
- [ ] On spectator connect: read current `GameSession` state
- [ ] Construct `GAME_STATE` message with current FEN, move history, clock state
- [ ] Deliver before any subsequent moves arrive (ordering is guaranteed within the goroutine)

### Step 4: Cross-Instance Delivery
- [ ] Verify `RedisEventBus` delivers move events to spectator's instance
- [ ] No code change expected — this validates Phase 2 architecture

### Step 5: Load Testing (Manual)
- [ ] Open 10 spectator tabs watching the same game
- [ ] Play 20 moves — verify all spectators receive all moves in correct order
- [ ] Disconnect 5 spectator tabs mid-game — verify remaining 5 continue correctly
- [ ] Verify move pipeline latency does not increase with 10 spectators (async delivery)

### Step 6: Documentation
- [ ] ARCHITECTURE.md: spectator registry, async fan-out diagram
- [ ] ADR: synchronous vs async spectator fan-out decision
- [ ] ADR: separate spectator endpoint vs single endpoint with role detection
- [ ] CLAUDE.md update

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | A spectator joining mid-game receives the correct current board state |
| 2 | 10 simultaneous spectators receive all moves in real time |
| 3 | A spectator disconnecting does not affect the game or other spectators |
| 4 | Move pipeline latency does not measurably increase with 10 spectators |
| 5 | Spectators on a different server instance (Phase 2 setup) receive moves correctly |
| 6 | All previous phase acceptance criteria still pass |

---

---

