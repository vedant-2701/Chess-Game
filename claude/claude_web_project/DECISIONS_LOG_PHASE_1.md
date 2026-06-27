# Architecture Decision Records

This log records every significant architectural decision, the alternatives considered, and why the chosen option was selected. Entries are append-only. When a decision is reversed, a new ADR supersedes the old one — the old one is not deleted. This preserves the reasoning history.

**Format:** ADR-XXX with status, context, options, decision, rationale, and consequences.

---

## ADR-001: Language Selection — Go

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need to select a backend language for a WebSocket-heavy, concurrent chess server. Primary goal is learning system design and distributed systems concepts. Secondary goal is building something production-grade.

**Options Considered:**

**Option A: Node.js (TypeScript)**
- Pros: Large ecosystem, chess.js library is mature, async/await is familiar, fast to get started, large community for WebSocket examples
- Cons: Concurrency model is hidden (event loop is implicit), async/await abstracts away what is actually happening, single-threaded limits conceptual transfer to distributed systems, TypeScript adds compile-time safety but not conceptual depth

**Option B: Go**
- Pros: Explicit concurrency via goroutines and channels (goroutine-per-connection model maps directly to distributed systems mental models), channels teach message-passing concepts that transfer directly to Redis pub/sub and message queues, stdlib is sufficient for most needs, compiled binary simplifies deployment, strong tooling (race detector is essential), HTTP and WebSocket primitives are close to the metal
- Cons: Requires learning a new language if unfamiliar, chess libraries are less mature than chess.js, steeper initial curve

**Option C: Python (FastAPI + asyncio)**
- Pros: Fast to prototype, asyncio teaches some concurrency concepts
- Cons: asyncio concurrency is implicit like Node.js, GIL limits true parallelism, WebSocket support is bolted on, not idiomatic for systems work

**Decision:** Go

**Rationale:**
The candidate has already built WebSocket infrastructure in Go: read loops, write loops, registries, heartbeats, and graceful shutdown with correct mutex usage and deadlock avoidance. Go is not a learning cost — it is an existing foundation. Additionally, Go's explicit goroutine model means that reasoning about "two players connected to different server instances" is a natural extension of "two goroutines sharing state," which the candidate already understands.

**Consequences:**
- Chess library (`notnil/chess`) is less mature than `chess.js`. Acceptable trade-off.
- Binary deployment is simpler than Node.js (no runtime required).
- Race detector (`go test -race`) is a first-class tool that will catch bugs automatically.

---

## ADR-002: WebSocket Library — gorilla/websocket

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need a WebSocket library. Candidate already has working infrastructure built on gorilla/websocket.

**Options Considered:**

**Option A: gorilla/websocket**
- Pros: Already in use, candidate understands it deeply, battle-tested at scale, explicit API (you see the upgrade, the frames, the close handshake)
- Cons: In maintenance mode (no new features), not pursuing RFC compliance updates

**Option B: nhooyr/websocket or coder/websocket**
- Pros: More actively maintained, context-aware API, slightly cleaner interface
- Cons: Switching cost with no learning benefit, candidate would need to re-implement proven infrastructure

**Option C: NestJS/Socket.IO equivalent (abandon Go)**
- Pros: Higher-level abstractions
- Cons: Abstractions are exactly what we are trying to avoid for learning

**Decision:** gorilla/websocket

**Rationale:**
The candidate has already proven they understand the low-level WebSocket protocol by building correct read/write loops, heartbeat handling, and graceful shutdown. Switching libraries loses that investment for marginal gain. gorilla/websocket being "in maintenance mode" is not a production risk — the WebSocket protocol does not change, and the library is stable.

**Consequences:**
- No new features from the library. Acceptable.
- Must implement ping/pong heartbeats manually. Already done.

---

## ADR-003: HTTP Router — go-chi/chi v5

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need HTTP routing for REST API endpoints (game creation, joining). WebSocket endpoint also needs routing.

**Options Considered:**

**Option A: net/http stdlib only**
- Pros: Zero dependencies, maximum learning
- Cons: No route parameters (/games/:id), no middleware chaining, too much boilerplate for diminishing return

**Option B: gin-gonic/gin**
- Pros: Most popular Go HTTP framework, large community, fast
- Cons: Uses its own handler type (`gin.HandlerFunc`) instead of `http.HandlerFunc`, creating framework lock-in. Middleware and handlers cannot be used outside Gin without adaptation.

**Option C: go-chi/chi v5**
- Pros: 100% stdlib-compatible (every handler is `http.HandlerFunc`), supports URL parameters, middleware chaining, no framework lock-in, can be dropped for pure stdlib with zero handler changes
- Cons: Smaller community than Gin, slightly less documentation

**Decision:** go-chi/chi v5

**Rationale:**
Framework lock-in is the primary concern. Gin's `gin.Context` wrapping means you are always writing to Gin, not to the stdlib interface. Chi handlers compile and run without Chi — they are just `http.HandlerFunc`. This means the routing layer can be replaced without changing any handler code. For a learning project, understanding stdlib-compatible HTTP is more valuable than Gin's convenience features.

**Consequences:**
- Handler signatures are always `func(w http.ResponseWriter, r *http.Request)`.
- No magic — request parsing and response writing are explicit.
- chi middleware is compatible with any `net/http` middleware.

---

## ADR-004: Database — PostgreSQL 16

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need persistent storage for game state, move history, and user identity.

**Options Considered:**

**Option A: PostgreSQL**
- Pros: ACID transactions (correct for game state — a move must not be half-applied), JSONB for flexible game metadata if needed, strong consistency required for chess, mature ecosystem, industry standard for relational workloads, `RETURNING` clause for clean insert-then-read patterns, advisory locks for Phase 3 matchmaking
- Cons: Requires running a database server

**Option B: SQLite**
- Pros: No server, embedded, simple for development
- Cons: No connection pooling, WAL mode has concurrency limits, not a realistic production database, teaches habits that don't transfer

**Option C: MongoDB**
- Pros: Flexible schema
- Cons: Chess game state is highly relational (games have moves, moves belong to games, users play games). A document store would require embedding moves in game documents, which creates unbounded document growth and makes move querying awkward. Chess is a relational problem.

**Option D: Redis as primary store**
- Pros: Fast, in-memory
- Cons: Not durable by default, not appropriate as a primary store for game state that must survive restarts

**Decision:** PostgreSQL 16

**Rationale:**
Chess game state is naturally relational. Every move belongs to exactly one game. Every game has exactly two players. ACID transactions guarantee that a move is either fully persisted or not persisted — there is no partial state. These are correctness requirements, not performance requirements. PostgreSQL is the correct choice.

**Consequences:**
- Requires Docker for local development.
- Connection pooling required (`pgxpool`).
- Migrations required (`golang-migrate`).

---

## ADR-005: Database Driver — pgx/v5 (pgxpool)

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need a PostgreSQL driver for Go.

**Options Considered:**

**Option A: database/sql with lib/pq**
- Pros: Standard interface, works with any SQL database
- Cons: `database/sql` is a lowest-common-denominator interface. Cannot use PostgreSQL-specific types natively (UUIDs, JSONB, arrays). Error handling is more verbose. Slower than pgx.

**Option B: pgx/v5 with pgxpool**
- Pros: Native PostgreSQL support, UUID types work correctly without string conversion, JSONB support, connection pooling built in, better error types, significantly faster than lib/pq, `RETURNING` clause support is clean
- Cons: PostgreSQL-only (acceptable — we are not switching databases)

**Option C: GORM or sqlx**
- Pros: Less boilerplate than raw SQL
- Cons: GORM hides queries, generates inefficient SQL, AutoMigrate is dangerous in production. sqlx is acceptable but adds a dependency with less benefit than pgx/v5 directly.

**Decision:** pgx/v5 with pgxpool

**Rationale:**
We are using PostgreSQL specifically. There is no reason to use a lowest-common-denominator interface. pgx/v5 exposes PostgreSQL properly and connection pooling via `pgxpool` is a requirement for a WebSocket server with concurrent games.

**Consequences:**
- All store code is PostgreSQL-specific. Acceptable.
- No ORM. All SQL is written explicitly. This is a feature, not a limitation — you must know your queries.

---

## ADR-006: Chess Move Validation Library — notnil/chess

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need chess move validation, FEN generation, game outcome detection. Considered writing from scratch.

**Options Considered:**

**Option A: Write custom move validation**
- Pros: Deep chess knowledge
- Cons: Not a system design exercise. En passant, castling rights tracking, promotion, fifty-move rule, threefold repetition — this is months of work that teaches chess programming, not distributed systems. An interviewer asking about system design is not impressed by a custom chess engine.

**Option B: notnil/chess**
- Pros: Handles all rules including edge cases, FEN parsing/generation, PGN, outcome detection, move generation
- Cons: Less mature than chess.js (Node.js equivalent), documentation is sparse

**Option C: Use chess.js via a subprocess call**
- Pros: Most mature chess library
- Cons: Cross-language subprocess calls are an antipattern for a hot path (every move). Latency and error handling complexity are not worth it.

**Decision:** notnil/chess

**Rationale:**
Chess domain logic is not the learning objective. Using a library is the correct engineering decision. The system design work is the WebSocket layer, state management, persistence, and scaling — not move validation.

**Consequences:**
- Wrap in `internal/chess/validator.go` to isolate the dependency.
- If `notnil/chess` has a bug or is abandoned, only the wrapper package changes.

---

## ADR-007: MVP Matchmaking Strategy — Shared Game Link

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
MVP needs a way for two players to start a game. Options are a matchmaking queue or a shared invite link.

**Options Considered:**

**Option A: Matchmaking queue**
- Pros: More realistic, teaches queue design
- Cons: Requires: queue data structure, pairing logic, race condition handling (two workers matching the same player), timeout handling, queue abandonment detection. Adds 30-40% complexity to Phase 1 with zero benefit to learning real-time WebSocket mechanics — which is Phase 1's actual learning goal.

**Option B: Shared game link**
- Pros: Player A creates a game, gets a URL with a gameID, shares it with Player B. Player B visits and joins. Simple. Correct. Defers queue complexity to Phase 3 where it is the primary learning objective.
- Cons: Not a realistic matchmaking experience. Acceptable for MVP.

**Decision:** Shared game link

**Rationale:**
Matchmaking is a separate system design concept that deserves its own phase. Combining it with Phase 1 (WebSocket mechanics) muddies both learning objectives. A shareable link solves the "two players need to find each other" problem with minimal complexity.

**Consequences:**
- Phase 1 scope is cleaner and more achievable.
- Matchmaking becomes Phase 3's primary learning objective.
- Users must share links out-of-band (WhatsApp, Discord, etc.). Acceptable for Phase 1.

---

## ADR-008: Authentication Strategy — JWT Player Tokens

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need player identity for WebSocket authentication and reconnection. Full account system is out of scope for Phase 1.

**Options Considered:**

**Option A: Full account system (username/password)**
- Pros: Realistic
- Cons: Out of scope. Auth implementation is not the learning objective. Would add session management, password hashing, email verification — none of which teach real-time systems design.

**Option B: Anonymous session cookies**
- Pros: Simple
- Cons: Cookies require HTTPS for security, cross-origin complications with WebSocket, session storage on server adds statefulness

**Option C: JWT player tokens, scoped per game**
- Pros: Stateless (server does not store sessions), carries all needed information (gameID, userID, color), works over WebSocket without cookie complications, enables reconnection without server-side session lookup, trivial to verify
- Cons: Token expiry means very long games could theoretically expire (solvable with generous expiry or refresh)

**Decision:** JWT player tokens scoped per game

**Rationale:**
A player token encodes `{ gameID, userID, color }`. On WebSocket connect, the client sends this token. The server verifies the signature and extracts the game context without any database lookup. On reconnection, the same token proves identity. This is stateless, fast, and correct.

**Consequences:**
- Token expiry must be set generously (24 hours or more) for long games.
- UserID is generated client-side on first visit (UUID stored in localStorage). This is anonymous identity, not authenticated identity.
- Phase 3+ will add proper user accounts as a separate concern.

---

## ADR-009: Registry Architecture — Two Separate Registries

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need to track both WebSocket connections and game sessions. Initially it seems like one registry could serve both purposes.

**Options Considered:**

**Option A: Single registry mapping connectionID to game**
- Pros: Simpler
- Cons: ConnectionID is ephemeral — it changes on reconnection. A single registry cannot answer "which connection is currently White in game X?" when White has just reconnected with a new connectionID. The connection-to-game mapping would need to be updated on every reconnection, creating coupling between infrastructure and game logic.

**Option B: Two registries with separate concerns**
- `ws.Registry`: `connectionID → *Connection` (infrastructure layer, ephemeral)
- `game.GameRegistry`: `gameID → *GameSession` (application layer, persistent for game lifetime)
- `GameSession` holds `*Connection` pointers, updated on reconnection
- Pros: Clean separation of concerns, reconnection is handled entirely at the game layer without touching the WebSocket infrastructure, the ws layer stays ignorant of game concepts
- Cons: Slightly more code

**Decision:** Two separate registries

**Rationale:**
The WebSocket infrastructure layer must not know about games. If it does, it cannot be reused for any other real-time feature. The game layer owns the concept of "Player White in game X is currently connected via connection Y." When White reconnects, only the game layer updates its pointer. The WebSocket layer just knows a new connection arrived.

**Consequences:**
- `ws.Registry` is a general-purpose connection tracker.
- `game.GameRegistry` bridges connections to game sessions.
- Reconnection logic lives entirely in `game.Manager`.

---

## ADR-010: EventBus Interface for Phase 2 Seam

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Phase 1 runs as a single process. Phase 2 requires cross-server WebSocket communication via Redis pub/sub. Without a seam, Phase 2 requires invasive refactoring of Phase 1 code.

**Options Considered:**

**Option A: Direct in-process function calls (no interface)**
- Pros: Simpler in Phase 1
- Cons: Phase 2 requires rewriting every place that broadcasts game events. The refactor is risky and touches core game logic.

**Option B: EventBus interface with LocalEventBus for Phase 1**
- Pros: Phase 2 is a dependency injection change only. `LocalEventBus` is replaced by `RedisEventBus` in `main.go`. No game logic changes.
- Cons: Slightly more indirection in Phase 1

**Decision:** EventBus interface from Phase 1

**Rationale:**
The cost of the interface in Phase 1 is minimal (one interface definition, one concrete implementation). The benefit in Phase 2 is avoiding a risky refactor of proven game logic. This is the standard "code to interfaces, not implementations" principle applied to a known future requirement.

**Consequences:**
- `internal/game/eventbus.go` defines the interface and local implementation from day one.
- Phase 2 adds `internal/game/redis_eventbus.go` and swaps at startup.
- All game event broadcasting goes through the EventBus, never directly to connections.

---

## ADR-011: No ORM — Raw SQL via pgx/v5

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
Need to decide whether to use an ORM (GORM, ent) or raw SQL.

**Decision:** Raw SQL via pgx/v5. No ORM.

**Rationale:**
ORMs hide queries. In a system design learning project, understanding what queries are being executed and why they are efficient or inefficient is a primary learning objective (Phase 4 specifically teaches database indexing via EXPLAIN ANALYZE). An ORM makes this opaque. GORM's AutoMigrate is dangerous in production. Raw SQL with `golang-migrate` for versioned migrations is the correct production pattern.

**Consequences:**
- All SQL is written by hand and reviewed deliberately.
- No generated queries that are hard to optimize.
- Migration files are explicit and reversible.
- More boilerplate in the store layer. Acceptable.

---

## ADR-012: No Framework — chi + stdlib

**Date:** 2025-06-23
**Status:** ACCEPTED

**Context:**
NestJS, Gin, and similar frameworks were considered.

**Decision:** go-chi/chi for routing. stdlib everywhere else. No application framework.

**Rationale:**
Frameworks abstract the concepts being learned. NestJS's `@WebSocketGateway()` hides WebSocket lifecycle. Gin's `gin.Context` hides `http.ResponseWriter`. The goal is to understand what happens, not to be productive with a framework. chi is not a framework — it is a router that is 100% stdlib-compatible.

**Consequences:**
- Handler code is portable and testable without the router.
- No framework-specific patterns to unlearn later.

---

## ADR-013: Chess Move Validation Strategy — Validate-Then-Apply Split

**Date:** 2026-06-25
**Status:** ACCEPTED

**Context:**
The move pipeline requires that in-memory board state must not change until a move is
successfully persisted to PostgreSQL. This is the persistence-first guarantee:
a move is only "real" once it is in the database. `notnil/chess` has no `UndoMove()`.
Once `game.MoveStr()` is called and returns nil, the mutation cannot be reversed.

The chess layer must expose an API that lets the pipeline persist first, then advance
board state — without requiring the pipeline to implement cloning or rollback itself.

**Options Considered:**

**Option A: FEN Round-Trip Clone**
`ValidateAndApply(game, san) (*chess.Game, error)` clones the game via
`game.Position().String()` (FEN serialization), applies the move to the clone,
and returns the clone. The pipeline replaces `session.board` only after DB write succeeds.

Flaw: FEN does not encode position history. `notnil/chess` detects threefold repetition
by comparing the current position to all prior positions in the game's internal `positions`
slice. A FEN-cloned game starts this slice from zero. After the first move applied via
clone-replace, `session.board` contains only one prior position. Threefold repetition
detection is silently broken for the lifetime of the game.

**Option D: Move Replay Clone (Option A corrected)**
Same pipeline semantics as Option A, but clones by replaying all moves from
`game.Moves()` onto a new game object rather than from FEN. Correct position history is
preserved. Still O(n) per move and allocates a new game object per move. Correct but
more complex than Option B for the same correctness guarantee.

**Option B: Validate-Then-Apply Split (CHOSEN)**
Expose two methods:
- `ValidateMove(game *chess.Game, san string) error` — checks legality, no mutation
- `ApplyMove(game *chess.Game, san string) error` — mutates game in-place, only call after successful DB write

`session.board` is the single continuous game object for the lifetime of the game.
All moves are applied to it sequentially. Position history is complete from game start.
Threefold repetition detection works correctly by construction.

**Option C: In-Place Mutation, Accept the Risk**
Rejected. Breaks the persistence-first guarantee. Not considered further.

**Decision:** Option B — Validate-Then-Apply Split

**Rationale:**
The primary correctness requirement is: in-memory board state must not advance past
the database. The secondary correctness requirement is: game outcome detection
(including threefold repetition) must be accurate.

Option B satisfies both requirements without cloning. `session.board` is mutated
exactly once per move, immediately after the DB write succeeds. Because `session.board`
is never replaced with a cloned object, the full position history accumulates correctly
in `notnil/chess`'s internal state across the entire game.

The TOCTOU concern (validate at time T, apply at time T+1, state could change between)
does not apply in this architecture. Each `GameSession` processes moves under a session
mutex (or a single goroutine per session). No other code path can modify `session.board`
between `ValidateMove` and `ApplyMove`.

Option D is also correct but pays O(n) per move and allocates a new `*chess.Game` per
move. These costs are irrelevant at chess move frequency, but the added complexity is
not justified when Option B achieves the same correctness with less code.

**API Change from Initial Spec:**
The PHASE_1.md spec suggested a single `ValidateAndApply(game, san) (*chess.Game, error)`
signature. This is replaced by two methods:
- `ValidateMove(game *chess.Game, san string) error`
- `ApplyMove(game *chess.Game, san string) error`

PHASE_1.md explicitly defers exact signatures to implementation time. This change is
within scope.

**Consequences:**
- Chess layer exposes `ValidateMove` and `ApplyMove` as separate methods on `*Validator`.
- The move pipeline calls them with the DB write between them. The persistence boundary
  is explicit in the pipeline code, not hidden inside the chess layer.
- `ValidateMove` and `ApplyMove` are independently testable without side effects.
- `ApplyMove` returning an error after `ValidateMove` returned nil is a bug, not an
  expected condition. It should be logged as an error with full context and is
  unrecoverable without reloading game state from the database.
- Threefold repetition detection works correctly for the full game duration.
- No object allocations on the move hot path beyond what `notnil/chess` does internally.
