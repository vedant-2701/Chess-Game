# Roadmap

This document defines the phased build plan for the chess server. Each phase builds on the previous one and is designed to teach a specific system design concept. Phases are not time-boxed. A phase is complete only when all acceptance criteria are met and the system is demonstrably working, not when the code is written.

**Current Phase: Phase 1 — MVP**

---

## Phase Overview

| Phase | Name | Core Concept | Status |
|-------|------|-------------|--------|
| 1 | MVP | WebSocket state management, server authority, session recovery | 🔄 In Progress |
| 2 | Horizontal Scaling | WebSocket scaling problem, Redis pub/sub | ⬜ Not Started |
| 3 | Matchmaking | Queue design, distributed locking, race conditions | ⬜ Not Started |
| 4 | ELO + Game History | Async processing, database indexing, deferred computation | ⬜ Not Started |
| 5 | Spectators | Fan-out write problem, broadcast at scale | ⬜ Not Started |
| 6 | Observability | Structured metrics, distributed tracing, operational readiness | ⬜ Not Started |

---

## Phase 1 — MVP

**System Design Concepts Taught:**
- WebSocket connection lifecycle (open, message, close, error, ping/pong)
- Server-authoritative game state (client is never trusted)
- Game state machine design (explicit states and transitions)
- Session management and identity without a full account system
- Stateful reconnection: restoring a player's game session after disconnect
- Persistence on the critical path: every move hits the database before being broadcast
- Concurrency-safe shared state (two goroutines, one game struct)

**What Is Built:**
- Two players play chess via a shared game link (no matchmaking)
- Game creation returns a unique link and a signed player token for White
- Joining the game returns a signed player token for Black
- Moves are validated server-side using notnil/chess
- Every move is persisted to PostgreSQL before being broadcast
- Game result detection: checkmate, stalemate, resignation
- Time controls: 10+0 (ten minutes, no increment) — single time control only
- Reconnection: player presents their token on reconnect and receives full board state
- Anonymous identity: userID is generated on first visit, stored client-side, signed into JWT

**What Is Explicitly Out of Scope for Phase 1:**
- Matchmaking (players always use shared links)
- ELO or rating system
- Game history browsing
- Spectators
- Chat
- Bots
- Multiple time controls
- Draw offers and draw acceptance (stalemate auto-detected, manual draw offer is Phase 4)
- Account registration or login
- Redis (all state is in-process + PostgreSQL)

**Acceptance Criteria:**
See [PHASE_1.md](./PHASE_1.md) for the complete checklist.

**Phase 1 is complete when:**
- Two browsers on different machines can complete a full chess game
- Closing and reopening the browser mid-game resumes the game correctly
- Killing and restarting the server mid-game resumes the game correctly (state from DB)
- An illegal move sent directly via WebSocket is rejected by the server
- A player running out of time results in a loss, detected server-side

---

## Phase 2 — Horizontal Scaling

**System Design Concept: The WebSocket Scaling Problem**

This is the most important distributed systems lesson in this project.

WebSocket connections are stateful. Player A is connected to Server Instance 1. Player B is connected to Server Instance 2. When Player A sends a move, Server 1 has no way to push it to Player B on Server 2 without an intermediary.

**The Problem You Will Hit:**
Deploy two instances of the Phase 1 server behind a load balancer. Two players connect. There is approximately a 50% chance they land on different instances. Their game breaks silently. Moves from one player never reach the other.

**What Is Built:**
- Redis pub/sub as the cross-server event bus
- Each server instance subscribes to game channels: `game:{gameID}`
- Move from Player A → Server 1 → validate → persist → publish to Redis channel
- Redis delivers to Server 2 → Server 2 pushes to Player B
- `EventBus` interface introduced in Phase 1 makes this a clean swap
- Sticky sessions considered and explicitly rejected (single point of failure per user)
- Load balancer configuration for WebSocket connections (connection upgrades, timeouts)

**What This Teaches:**
- Why stateful services cannot be naively horizontally scaled
- Redis pub/sub semantics: fire-and-forget, at-most-once delivery
- The difference between stateful infrastructure (WebSocket servers) and stateless infrastructure
- What happens when Redis goes down: graceful degradation design

**Phase 2 is complete when:**
- Two server instances run behind nginx
- Players on different instances can play a complete game
- Killing one server instance mid-game: player reconnects to remaining instance, game resumes

---

## Phase 3 — Matchmaking

**System Design Concept: Queue Design and Distributed Race Conditions**

**What Is Built:**
- Player enters a matchmaking queue (single time control: 10+0)
- Server pairs players by wait time (simple FIFO for Phase 3, ELO-based in Phase 4)
- Matched players are notified via WebSocket and redirected to their game
- Redis sorted sets used for queue management
- Distributed locking to prevent two server instances from matching the same player twice

**What This Teaches:**
- Queue-based system design
- Race conditions in distributed systems: two workers dequeuing the same item
- Atomic operations in Redis (ZPOPMIN is atomic; naive read-then-delete is not)
- When a feature becomes a separate service (matchmaking vs game server)
- Backpressure: what happens when the queue grows faster than matches are made

**Phase 3 is complete when:**
- Two players entering the queue are matched and a game is created automatically
- No player is ever matched with themselves
- No player is ever matched twice simultaneously (race condition proof)
- A player leaving the queue (closing browser) is correctly removed

---

## Phase 4 — ELO and Game History

**System Design Concept: Deferred Async Processing and Query Optimization**

**What Is Built:**
- ELO rating computed after every completed game
- Game history browsable per player (list of games with outcomes)
- PGN export for any game
- ELO computation is async: game ends → event emitted → worker computes ELO
- Database indexes added based on observed query patterns

**What This Teaches:**
- Why ELO should not be computed synchronously on the game-end request
- Job queue pattern: fire-and-forget vs at-least-once delivery
- What happens when the ELO worker crashes mid-computation (idempotency)
- PostgreSQL index design: understanding EXPLAIN ANALYZE output
- The difference between write-path optimization and read-path optimization

**Phase 4 is complete when:**
- ELO updates after every game without blocking game completion
- A player's ELO history is queryable efficiently (sub-10ms for 10,000 games)
- A failed ELO computation retries and does not double-apply

---

## Phase 5 — Spectators

**System Design Concept: Fan-out Write Problem**

**What Is Built:**
- Any number of users can watch a live game
- Spectators receive every move in real time
- Spectators can join mid-game and receive the current board state immediately
- No spectator can interact with the game

**What This Teaches:**
- Fan-out: 1 write (a move) must produce N reads (push to N spectators)
- WebSocket connection count management
- The cost of synchronous fan-out: if broadcasting to 1000 spectators synchronously blocks move processing, latency explodes
- Async fan-out via goroutines or channels
- Connection pool management: spectators who disconnect must be cleaned up

**Phase 5 is complete when:**
- 10 simultaneous spectators receive moves in real time
- A spectator joining mid-game receives the correct current board state
- A spectator disconnecting does not affect the game or other spectators

---

## Phase 6 — Observability

**System Design Concept: You Cannot Fix What You Cannot Measure**

This phase is not about adding features. It is about making the system understandable in production.

**What Is Built:**
- Structured logging with request IDs propagated across all layers
- Prometheus metrics: active connections, active games, move processing latency (p50/p95/p99), game outcomes per hour, matchmaking queue depth
- Health check endpoint: `/health` (liveness) and `/ready` (readiness)
- Graceful shutdown completing all in-flight moves before exit
- Runbook: what to do when each alert fires

**What This Teaches:**
- The difference between logging (what happened) and metrics (how often / how long)
- Why p99 latency matters more than average latency for real-time systems
- Health check design: liveness vs readiness distinction
- Operational readiness: a system is not production-ready until you can debug it without reading code

**Phase 6 is complete when:**
- Every error log contains: requestID, gameID, playerID, error message, stack context
- Move processing p99 latency is measurable and below 100ms under normal load
- Server can be killed and restarted with zero game state loss

---

## What This Project Does Not Teach

Be explicit about gaps. If you need to learn these concepts, this project is not the vehicle:

- **High write throughput**: Chess is a low-frequency system. You will not learn sharding, write amplification, or backpressure from a chess game.
- **Complex fan-out at scale**: Twitter-scale fan-out (1 write → millions of reads) requires a different class of system.
- **Search and indexing at scale**: No full-text search, no recommendation engine, no feed ranking.
- **CDN and content delivery**: No media assets.
- **Eventual consistency**: Chess requires strong consistency. You will not learn how to design systems that tolerate inconsistency.
- **Payment systems**: Not applicable.
- **Multi-region deployment**: Beyond the scope of this learning project.
