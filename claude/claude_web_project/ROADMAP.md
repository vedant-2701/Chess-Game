# Roadmap

This document defines the phased build plan for the chess server. Each phase builds on the previous one and is designed to teach a specific system design concept. Phases are not time-boxed. A phase is complete only when all acceptance criteria are met and the system is demonstrably working, not when the code is written.

**Current Phase: Phase 2 — Horizontal Scaling**

> Phase 2's design was substantially reworked after the originally-planned Redis
> pub/sub approach was audited against the actual codebase and found to have
> critical bugs before implementation began. See `DECISIONS_LOG_PHASE_2.md`
> (ADR-021 through ADR-025) for the full reasoning, and `phases/current/PHASE_2.md`
> for the current design. `phases/current/PHASE_2-deferred.md` preserves the
> original plan for history.

---

## Phase Overview

| Phase | Name | Core Concept | Status |
|-------|------|-------------|--------|
| 1 | MVP | WebSocket state management, server authority, session recovery | ✅ Complete |
| 2 | Horizontal Scaling | Instance co-location, ownership/liveness routing directory | 🔄 In Progress (design complete, implementation not started) |
| 3 | Matchmaking | Queue design, distributed locking, race conditions | ⬜ Not Started |
| 4 | ELO + Game History | Async processing, database indexing, deferred computation | ⬜ Not Started |
| 5 | Spectators | Fan-out write problem, broadcast at scale | ⬜ Not Started |
| 6 | Observability | Structured metrics, distributed tracing, operational readiness | ⬜ Not Started |
| 7 | Frontend | Browser WebSocket lifecycle, client-server state sync | ⬜ Not Started |
| 8 | Container Orchestration | Kubernetes, consensus-backed service discovery | ⬜ Not Started |

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

**Status: ✅ COMPLETE.** All 10 acceptance criteria met — see `CLAUDE.md`'s Phase 1
Completion Record for the criterion-by-criterion account.

---

## Phase 2 — Horizontal Scaling

**System Design Concept: Routing Stateful Sessions to Their One Correct Owner**

This is the most important distributed systems lesson in this project — the
specific lesson changed during design, and it's worth being explicit about why.

The originally-planned lesson was "Redis pub/sub lets two independent processes
share one game's state." That framing was wrong. A chess game is a 1:1,
turn-based session with exactly one legitimate owner at a time — there is no
correctness benefit to two processes each holding live state for the same game,
only correctness *risk*. This was discovered by auditing the original design
against the actual codebase before writing any implementation: the two-owner
model produced a deterministic cross-instance activation deadlock and several
other critical bugs (full list: `DECISIONS_LOG_PHASE_2.md` ADR-021). The
corrected lesson: **co-locate a game's two connections on one instance always,
and solve routing — getting a connection to the right owner — as a separate,
simpler problem from state synchronization.**

**The Problem You Will Hit:**
Deploy two instances of the Phase 1 server behind a load balancer with no
routing awareness. Two players connect. There is roughly a 50% chance they land
on different instances. Their game breaks silently.

The naive fix — encode routing into the gameID, or hash-route at the load
balancer — trades this for a subtler failure: it works until an instance
crashes, fails over, and then *recovers*. The recovered instance has no memory
of the failover and will happily reconstruct its own independent copy of a game
that already migrated elsewhere — splitting a live game via the very mechanism
meant to prevent that. Both of these were found and rejected during design
(`DECISIONS_LOG_PHASE_2.md` ADR-021) before being built.

**What Is Built:**
- Redis-backed routing directory: two keys per game/instance —
  `game:{gameID} → instanceID` (ownership, stable) and
  `instance_alive:{instanceID}` (liveness, the actual failure-detection signal)
  — deliberately decoupled, not one combined key (ADR-023)
- Resolve-then-connect: a stateless `GET /games/:id/resolve` call determines
  the correct instance and issues a short-lived, masked routing credential
  *before* the WebSocket upgrade is attempted — no server-side connection
  relay/proxy code exists anywhere (ADR-022)
- `LocalEventBus` (from Phase 1) remains the permanent implementation.
  `RedisEventBus` is not built — there is never a second process holding state
  for the same game that needs cross-instance notification
- nginx: plain round-robin for REST; a static label→upstream map for the
  WebSocket Edge Proxy — mechanical dereference only, no routing decisions made
  at the proxy layer
- TD-008 (automatic migrations racing under multiple instances, open since
  Phase 1) closed via a one-shot pre-deploy migration step (ADR-025)

**What This Teaches:**
- Why stateful services cannot be naively horizontally scaled — and why the
  right response is routing to a single owner, not synchronizing multiple owners
- Lease-based ownership and liveness: TTLs, heartbeats, and the concrete
  false-positive/false-negative tradeoff every lease design has to make
  explicitly (ADR-023) — and why that tradeoff is deliberately left as a
  documented, bounded limitation (TD-P2-001) rather than fully solved by hand
- The difference between a stateless routing decision (resolve) and a stateful
  long-lived session (connect) — and why separating them removes a whole class
  of implementation risk
- What happens when the routing store goes down: already-connected players are
  unaffected; only new routing decisions fail, and they fail cleanly

**Phase 2 is complete when:** see `phases/current/PHASE_2.md`'s Acceptance
Criteria table — in particular, criterion 4: a failed-over game's original
instance recovering must never cause a split game, under any timing.

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

## Phase 7 — Frontend

**System Design Concept: Browser WebSocket Lifecycle, Client-Server State Synchronization**

**What Is Built:**
- Next.js 14+ App Router frontend deployed to Vercel
- Browser native WebSocket connecting directly to Go server (no proxy, no Socket.IO)
- Chess board via react-chessboard, client-side move validation via chess.js (UX only)
- Game state driven entirely by server WebSocket messages via useReducer
- Reconnection with exponential backoff matching the server's abandonment window
- Game history, spectator view, matchmaking UI, game analysis (step through moves)
- CORS configured on Go server for Vercel domain
- WebSocket origin validation tightened on Go server

**What This Teaches:**
- Browser WebSocket lifecycle: the same connection management problems exist on the client as on the server
- The boundary between server components (SSR, no browser APIs) and client components (useState, WebSocket) in Next.js App Router — getting this wrong causes hydration errors
- Optimistic UI updates and rollback: move shows immediately, reverts on server rejection
- Client clock as cosmetic only: server is authoritative on timeout, client only renders the countdown
- Token storage strategy for reconnection: localStorage keyed by gameID
- CORS as a real backend concern, not just a development annoyance

**Tech Stack:**
Next.js 14+, TypeScript, Tailwind CSS, react-chessboard, chess.js, zod, native fetch, native WebSocket. No Redux, no Zustand, no Socket.IO.

**Deployment:**
Next.js on Vercel (free tier). Go server on VPS (existing). WebSocket connects directly from browser to Go server — Next.js does not proxy WebSocket connections.

**Phase 7 is complete when:**
- Two players on different machines complete a full game via the browser UI
- Reconnection works in production: close tab, reopen, game resumes
- All functionality works on production Vercel + VPS URLs, not just localhost

---

## Phase 8 — Container Orchestration

**System Design Concept: Hand-Rolled Primitive vs. Platform-Provided Primitive**

Added during Phase 2's design, not part of the original plan — deliberately
deferred rather than pulled into Phase 2 itself. See `DECISIONS_LOG_PHASE_2.md`
ADR-021's context section for why: the lesson Phase 2 teaches (how lease-based
ownership routing works) is best learned on the simplest tool that teaches it
(Redis, two commands) before adopting a platform that solves it more completely
but requires learning the platform's own machinery at the same time.

**What Is Built:**
- Redeploy the Phase 2 fleet onto Kubernetes (local cluster — kind or minikube;
  no cloud provider)
- Replace nginx's static Edge Proxy map with a `Service` + `ingress-nginx`
- Replace the hand-rolled Redis ownership/liveness directory with Kubernetes'
  native `Lease` API object — same TTL/claim concept as Phase 2, now
  Raft-backed via etcd instead of a single Redis instance, with
  `resourceVersion`-based fencing closing Phase 2's TD-P2-001
- `StatefulSet` for stable per-instance identity, replacing Phase 2's manually
  assigned `INSTANCE_ID`
- Liveness/readiness probes replacing nginx's passive health checks
- `ConfigMap`/`Secret` replacing `.env`-file configuration

**What This Teaches:**
- Hand-rolled lease (Phase 2, Redis) vs. consensus-backed lease (k8s `Lease`/etcd)
  — same concept, stronger guarantee, understood in depth because the weaker
  version was built by hand first
- Declarative reconciliation vs. imperative deployment
- Fencing tokens: why Phase 2 explicitly accepted a bounded correctness risk
  instead of hand-building this, and what closing it with the right primitive
  actually looks like

**Explicitly Out of Scope:** service mesh (Istio/Linkerd), multi-cluster/
multi-region, autoscaling policy tuning.

**Phase 8 is complete when:** see `phases/future/Phase_8.md`'s full acceptance
criteria, in particular TD-P2-001's closure test.

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
