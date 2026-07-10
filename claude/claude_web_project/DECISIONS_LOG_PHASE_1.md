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

---

## ADR-014: TOCTOU Race — ProcessMove vs. Concurrent Terminal-State Transitions

**Date:** 2026-06-30
**Status:** ACCEPTED

**Context:**
`ProcessMove` checks `session.status == ACTIVE` at the top of the pipeline via
`CurrentStateSnapshot()` (which acquires and releases `session.mu.RLock`), then proceeds
through `SaveMove` → `UpdateCurrentFEN` → `ApplyMove` without re-checking status.
`ApplyMove` acquires `session.mu.Lock()` to mutate `session.board`, but the lock
acquisition and the status check are decoupled — separated by two DB writes.

Meanwhile, `handleResign`, `handleTimeout`, and `onAbandonTimeout` can call
`session.Transition(COMPLETED/ABANDONED)` from a different goroutine (the opponent's
WebSocket read loop, the Clock's background goroutine, or the abandon timer goroutine
respectively). `Transition` acquires `session.mu.Lock()` and changes `session.status`.

If Transition fires between ProcessMove's initial status check and its ApplyMove lock
acquisition, ProcessMove will:
- Persist an orphaned move row for a game that is already logically over
- Mutate `session.board` past the terminal position
- Publish a `MOVE_APPLIED` event after `GAME_OVER` has already been broadcast

The orphaned DB row is inert (RestoreActiveGames never loads terminal-state games). The
board mutation is harmless (no further reads will occur). The out-of-order broadcast is
a real protocol violation that can confuse clients.

**Options Considered:**

**Option A: Re-check status under the ApplyMove write lock (CHOSEN)**
Expand the existing `session.mu.Lock()` acquisition for `ApplyMove` to include a status
check: if `session.status != ACTIVE`, skip ApplyMove, clock switch, outcome detection,
and broadcast. Return nil — the game ended via a legitimate concurrent path.

~5 lines of code. No new architecture. Orphaned `SaveMove` row (which ran before the
lock) is inert: COMPLETED/ABANDONED games are never replayed by RestoreActiveGames, so
a trailing move row has no downstream effect.

- Pros: Minimal change, uses existing lock, closes the exact race window, no latency
  coupling with DB operations.
- Cons: Does not prevent the orphaned DB row. Does not prevent future races at other
  points in the session lifecycle (though no other races are currently identified).

**Option B: Single-writer-per-session (actor model)**
Route every mutating operation (MOVE, RESIGN, clock timeout, abandon timeout) through a
single goroutine per session via a channel. Eliminates concurrent entry into session
state by construction.

- Pros: Eliminates all intra-session races by construction, not just this one. Makes
  the "single goroutine per session" comment in move.go a structural truth rather than
  an aspirational comment. Textbook correct for the problem domain.
- Cons: Architectural change touching MoveProcessor, all three Manager terminal-state
  handlers, and how Step 11's WebSocket read loops hand off to the game layer. Over-
  engineering relative to the actual bug count (one race, plus the timeout race that
  Transition already handles). Introduces channel backpressure concerns and complicates
  error propagation from the actor goroutine back to the caller.

**Option C: Conditional DB write (UPDATE ... WHERE status = 'ACTIVE')**
Make the database the authoritative coordination gate via row-level conditional update.

- Pros: Database-level consistency even in multi-instance deployments.
- Cons: Solves a Phase-2 problem that does not exist in Phase 1's single-process model.
  More fundamentally, the in-memory session is the authoritative state for in-flight
  game logic — making the DB a coordination mechanism inverts that design and adds DB
  latency to the critical path of every mutating operation. Phase 2's multi-instance
  coordination is solved by Redis pub/sub and distributed locking, not by turning
  PostgreSQL into a mutex.

**Option D: Hold session.mu.Lock() across the entire post-validation pipeline**
Acquire the write lock before SaveMove and hold it through ApplyMove, clock switch,
outcome detection, and broadcast.

- Pros: Eliminates the race window entirely.
- Cons: Lock is held during DB writes (SaveMove, UpdateCurrentFEN, UpdateClocks),
  coupling session lock hold time to PostgreSQL latency. All other session operations
  (including CurrentStateSnapshot for reconnecting players) block during DB round-trips.
  Trades a narrow race for a potential latency bottleneck.

**Decision:** Option A — Re-check status under the ApplyMove write lock.

**Rationale:**
The race window is between ProcessMove's initial status check and its `session.mu.Lock()`
acquisition for ApplyMove. Option A closes this window at exactly the point where the
symptom occurs: the in-memory mutation and broadcast. The fix is consistent with how
`handleTimeout` already handles the symmetric race — Transition is the gate, and the
loser no-ops.

The orphaned `SaveMove` row is provably inert. `RestoreActiveGames` only loads ACTIVE
and WAITING games via `GetActiveGames`. A COMPLETED or ABANDONED game will never have
its move rows replayed. The trailing move is invisible to all current and planned
consumers of the data.

Option B (actor model) is architecturally superior but unjustified by the current bug
count: exactly one race (this one), plus the timeout race that Transition already
handles. Introducing an actor model as a quiet bug fix would be over-engineering. If
Step 11 testing under real concurrent read loops (with `-race`) surfaces additional
intra-session races, Option B should be revisited as a dedicated ADR.

Option C is rejected as the wrong abstraction layer. Option D is rejected as trading a
narrow race for a latency bottleneck.

**Consequences:**
- ProcessMove's ApplyMove section is expanded from:
  ```go
  session.mu.Lock()
  applyErr := p.validator.ApplyMove(session.board, san)
  session.mu.Unlock()
  ```

to:

```go
session.mu.Lock()
if session.status != store.GameStatusActive {
    session.mu.Unlock()
    slog.Info("ProcessMove: game no longer ACTIVE after DB write — skipping apply",
        "gameID", session.ID, "san", san, "status", session.status)
    return nil
}
applyErr := p.validator.ApplyMove(session.board, san)
session.mu.Unlock()
```

- No changes to Manager, terminal-state handlers, EventBus, or Clock.
- The orphaned SaveMove row is accepted as a known, inert artifact. If future phases require move-table cleanliness (e.g., replay features, move analytics), a migration or cleanup job can be added then.
- Option B (actor model) is flagged for ADR discussion before or during Step 11 if -race testing under concurrent read loops reveals further intra-session races.

---

## ADR-015: Abandonment Semantics Correction — Single vs. Both Players Disconnected

**Date:** 2026-06-30
**Status:** ACCEPTED

**Context:**

`PHASE_1.md`'s original Game State Machine section, and `manager.go`'s initial
`onAbandonTimeout` implementation (Step 10), specified and implemented abandonment as
follows: when a player disconnects, a 60-second timer starts; if the timer fires
without reconnection, the game unconditionally transitions to `ABANDONED`.

This is wrong as both a spec and an implementation. As implemented, the condition for
`ABANDONED` never actually required both players to be disconnected — the timer fires
based on a single player's disconnect duration, with no check of the opponent's
connection state. The practical consequence: if Player A's connection drops for any
reason exceeding 60 seconds while Player B remains actively connected and waiting, the
game is incorrectly marked `ABANDONED` (a draw) out from under Player B, even though
B never abandoned anything and was actively present the entire time. Conversely, the
spec's literal wording ("both players disconnected AND at least one did not reconnect")
describes a condition that, if actually checked as written, would mean a single
permanently disconnected player with a connected opponent would leave the game stuck
`ACTIVE` forever — neither the written spec nor the as-implemented code produced fully
correct behavior.

This was caught during code review (not via a failing test — no Manager tests exist
yet) by reading `manager.go`'s `onAbandonTimeout` directly and checking it against the
stated PHASE_1.md state machine.

**Options Considered:**

**Option A: Keep both-disconnected-only abandonment, fix the missing check (literal spec)**
Make `onAbandonTimeout` actually verify both players are disconnected before
transitioning to `ABANDONED`; if only one is disconnected, take no action and leave
the game `ACTIVE` indefinitely.

- Pros: Matches the literal original spec wording exactly.
- Cons: A single player who disconnects and never returns (closed laptop, lost phone,
  gave up) leaves their opponent stuck in an `ACTIVE` game forever, with the clock
  paused (TD-002) and no path to resolution. Worse user experience than real chess
  platforms provide, and contradicts the project's own framing of abandonment as
  something that should resolve a stuck game, not describe one precondition for a
  rarely-reachable terminal state.

**Option B: Single-player disconnect triggers abandonment-loss; both-disconnected triggers a drawn abandonment (CHOSEN)**
Distinguish two abandonment outcomes based on the opponent's connection state at the
moment the 60-second timer fires:
  - Opponent connected → disconnected player loses by abandonment. `ACTIVE → COMPLETED`,
    `outcome` = opponent's color, `outcome_reason: ABANDONED`.
  - Opponent also disconnected → true mutual abandonment. `ACTIVE → ABANDONED`,
    `outcome: DRAW`, `outcome_reason: ABANDONED`.

- Pros: Resolves the common case (one player's connection drops, the other is left
  waiting) the way real-time multiplayer games typically handle it — the present
  player is not penalized by being stuck in limbo. Still correctly handles true mutual
  abandonment as a draw, matching the original spec's intent for that specific case.
  No new DB schema or outcome_reason value needed — `ABANDONED` as an `outcome_reason`
  already supports pairing with either a winner outcome or `DRAW` per the existing
  `games` table CHECK constraint.
- Cons: `outcome_reason: ABANDONED` is no longer sufficient on its own to determine
  whether the game was a decisive result or a draw — clients must read `outcome`
  (`WHITE`/`BLACK` vs `DRAW`) together with `status` (`COMPLETED` vs `ABANDONED`) to
  fully interpret an abandonment-ended game. Minor protocol nuance, not a new field or
  migration.

**Option C: Asymmetric timers — shorter timeout for single-disconnect-loss, longer for mutual-abandonment-draw**
Use two different timer durations: e.g. 60s for single-player abandonment-loss, but a
longer window (e.g. 120s) before declaring mutual abandonment a draw, on the theory
that a draw is a more drastic/final outcome and deserves more grace.

- Pros: Slightly more lenient for the both-disconnected edge case.
- Cons: Two timer durations is added complexity with no clear product requirement
  driving it. PHASE_1.md specifies a single 60-second window; introducing a second
  duration is unscoped complexity for Phase 1, not something requested. Rejected on
  the same grounds the project rejects other speculative complexity (see ROADMAP.md's
  "What This Project Does Not Teach" and the "no just-in-case hooks" instruction in
  PHASE_1.md's Explicitly Out of Scope section).

**Decision:** Option B — single-player disconnect triggers abandonment-loss
(`COMPLETED`, opponent wins); both-disconnected triggers a drawn abandonment
(`ABANDONED`).

**Rationale:**

The core problem with the original spec and implementation is that they conflated two
genuinely different scenarios under one mechanism. "My opponent vanished and I'm stuck
waiting" and "we both got disconnected at the same time" are different situations with
different correct outcomes — the first has a clear winner (whoever stayed), the second
does not. Option B is the only one of the three that resolves both situations to a
correct, non-stuck terminal state without introducing unscoped complexity (Option C)
or leaving a real failure mode unaddressed (Option A).

This correction was made to the documentation (`PHASE_1.md`, `ARCHITECTURE.md`) and the
implementation (`manager.go`'s `onAbandonTimeout`, plus a new `GameSession.IsPlayerConnected`
method) in the same session, per the project's spec-first discipline: the doc correction
is recorded here and in both spec files before being treated as settled.

**Consequences:**

- `GameSession` gains `IsPlayerConnected(color store.Color) bool`, a read-locked check
  of the relevant connection slot. Used exclusively by `onAbandonTimeout` to determine
  the opponent's connection state at the moment the timer fires.
- `Manager.onAbandonTimeout` branches on `session.IsPlayerConnected(opponentOf(color))`:
  connected → `Transition(COMPLETED)` with the opponent as winner; not connected →
  `Transition(ABANDONED)` with a `DRAW` outcome. Both branches set
  `outcome_reason: ABANDONED` — the `status` field (`COMPLETED` vs `ABANDONED`), not
  `outcome_reason`, is what distinguishes a decisive abandonment-loss from a drawn
  mutual abandonment.
- No database schema or migration change required. `outcome` already supports
  `WHITE`/`BLACK`/`DRAW` and `outcome_reason` already supports `ABANDONED` independent
  of which `outcome` it pairs with.
- `PHASE_1.md`'s WebSocket Message Protocol section (`GAME_OVER` reason list) and Game
  State Machine section, and `ARCHITECTURE.md`'s Game State Machine and WebSocket
  Connection Lifecycle sections, are updated with the corrected behavior.
- This correction was made in the same session as ADR-014 (TOCTOU re-check) and the
  `finalizeGame` registry-cleanup centralization (see Implementation Decisions in
  CLAUDE.md). All three address gaps found by direct code review of `manager.go` and
  `move.go` against `PHASE_1.md`/`ARCHITECTURE.md`, not by failing tests — Manager has
  no test coverage yet. This is itself a reason Manager integration tests, including
  explicit coverage of both `onAbandonTimeout` branches, are scheduled immediately
  following this ADR, before Step 11.

---

## ADR-016: JoinGame Double-Join Race — Atomic Conditional UPDATE

**Date:** 2026-07-01
**Status:** ACCEPTED

**Context:**

While writing Manager integration tests (immediately following ADR-015), a test for
rejecting an already-joined game (`TestManager_JoinGame_RejectsWhenAlreadyJoined`)
failed against the original `JoinGame` implementation. `JoinGame`'s only precondition
check was `game.Status != store.GameStatusWaiting`. Per this session's own confirmed
design (status remains `WAITING_FOR_PLAYER` until both WebSocket connections go live
in `HandleConnect`, not on the HTTP join call), that check alone could not detect
"this game already has a Black player assigned." A second, different user calling
`JoinGame` before any WebSocket connected would pass the status check and silently
overwrite `player_black_id` in both the DB and the in-memory `GameSession`.

The first fix applied — adding `if game.PlayerBlackID != nil { return ErrGameNotJoinable }`
to `JoinGame`'s pre-flight check — closed the *sequential* case (second join arriving
after the first has already committed) but not the *concurrent* case. `JoinGame`'s
sequence was read-then-write across two separate statements with no transaction or
locking between them:

```go
game, err := m.gameStore.GetGame(ctx, gameID)      // READ
if game.PlayerBlackID != nil { return ... }          // CHECK (in application code)
...
m.gameStore.UpdatePlayerBlack(ctx, gameID, userID)   // WRITE — unconditional SQL
```

`UpdatePlayerBlack`'s original SQL was `UPDATE games SET player_black_id = $1 ...
WHERE id = $2` — no predicate on the game's current state. Two different users calling
`JoinGame` for the same `gameID` concurrently could both execute `GetGame` and observe
`player_black_id IS NULL` before either committed its `UpdatePlayerBlack`. Whichever
`UPDATE` executed last would win silently, with no error, no conflict signal, and the
loser believing they had successfully joined (their JWT is validly signed regardless).
This is a classic application-level check-then-act race resolved against a database
that was never asked to enforce the invariant itself. It is invisible to `go test -race`
since the race is at the database level, not a Go memory race — confirmed by writing
`TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins`, which reproduces the race with
two real goroutines against a live PostgreSQL instance and only passes once the fix
below is applied.

**Options Considered:**

**Option A: Row-level lock via `SELECT ... FOR UPDATE`**
Wrap `GetGame` + `UpdatePlayerBlack` in an explicit transaction, using
`SELECT ... FOR UPDATE` to lock the row before the check-then-act sequence.

- Pros: Keeps the read-then-write shape of the code close to its current form;
  the locking is explicit and visible at the call site.
- Cons: Requires `JoinGame` to manage a `pgx.Tx` directly, which does not fit the
  current `GameStore`/`MoveStore` method-per-operation shape used everywhere else in
  `internal/store` (see ADR-011, no-ORM raw-SQL discipline — introducing ad hoc
  transaction management in `internal/game` would leak persistence-layer concerns
  upward, violating `ARCHITECTURE.md`'s stated rule that no SQL, and by extension no
  transaction control, lives outside `internal/store`). Also holds a row lock for the
  duration of the Go-level round trip between the `SELECT` and the `UPDATE`, which is
  strictly worse under contention than a single atomic statement.

**Option B: Atomic conditional UPDATE (CHOSEN)**
Make the `UPDATE` itself the authoritative check, via a `WHERE` clause encoding the
full precondition, and treat `RowsAffected() == 0` as failure:

```sql
UPDATE games
SET player_black_id = $1, updated_at = NOW()
WHERE id = $2 AND status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL
```

- Pros: PostgreSQL evaluates the `WHERE` predicate and performs the write atomically
  within a single statement — there is no window between check and act for a second
  transaction to interleave. No explicit transaction or lock management needed in Go
  code. Stays entirely within `GameStore`'s existing method-per-operation shape;
  `internal/game` still never touches SQL. The prior `GetGame` pre-flight check in
  `JoinGame` remains useful for producing a specific, friendly error (`ErrSelfPlay` vs
  `ErrGameNotJoinable`) in the common non-racing case, but is no longer the source of
  the actual correctness guarantee — the UPDATE is.
- Cons: `RowsAffected() == 0` is ambiguous on its own — it fires whether the row is
  genuinely missing or exists but failed the predicate. Requires a new error sentinel
  to disambiguate (see Consequences).

**Option C: Advisory lock (`pg_advisory_xact_lock`) keyed on gameID**
Acquire a PostgreSQL advisory lock for the duration of the join operation.

- Pros: General-purpose serialization primitive, would also cover future
  multi-statement operations on the same game row.
- Cons: Solves a more general problem than the one that exists. `JoinGame` has
  exactly one write; a general-purpose distributed-locking primitive is unscoped
  complexity for Phase 1 (this project explicitly avoids "just in case" hooks per
  PHASE_1.md's Explicitly Out of Scope section). Advisory locks also require careful
  session/transaction-scoping discipline to avoid being held longer than intended —
  another source of subtle bugs for a problem a single SQL predicate already solves.

**Decision:** Option B — atomic conditional UPDATE with a `WHERE` clause encoding
the full join precondition, disambiguated from "not found" via a new
`store.ErrGameNotJoinable` sentinel.

**Rationale:**

The correctness requirement is narrow: exactly one of two racing writers should
succeed, and PostgreSQL already provides this for free at the single-statement level
— a `WHERE` clause is evaluated and the write applied atomically per row, with no
additional locking primitive required. Reaching for explicit transactions (Option A)
or advisory locks (Option C) would solve the same problem with more moving parts and,
in Option A's case, would also violate the project's own layering rule that
transaction/SQL control stays inside `internal/store`. Option B keeps the fix entirely
within `GameStore.UpdatePlayerBlack`'s existing shape — no new dependency, no new
concurrency primitive, no change to any call site's calling convention beyond error
handling.

The original bug in the fix's first pass — returning `store.ErrGameNotFound` on
`RowsAffected() == 0` — was itself a violation of `CODING_GUIDELINES.md` §1's
"distinguish not-found from error" rule: a row that exists but fails a precondition is
not the same failure mode as a row that does not exist, and conflating them produces a
misleading error for the legitimate loser of a join race (who would see "game not
found" for a game that plainly exists and that they can see on screen). `store.ErrGameNotJoinable`
was added to `internal/store/errors.go` rather than reusing `game.ErrGameNotJoinable`
directly, because `internal/store` must not import from `internal/game` —
`ARCHITECTURE.md`'s dependency graph is strictly `game → store`, never the reverse.
`Manager.JoinGame` translates `store.ErrGameNotJoinable` to `game.ErrGameNotJoinable`
via `errors.Is` at the package boundary, so no caller outside `internal/store` ever
depends on a store-package sentinel.

**Consequences:**

- `GameStore.UpdatePlayerBlack`'s SQL now reads:
  `UPDATE games SET player_black_id = $1, updated_at = NOW() WHERE id = $2 AND status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL`.
- `store.ErrGameNotJoinable` added to `internal/store/errors.go`, returned by
  `UpdatePlayerBlack` when `RowsAffected() == 0` (row exists, precondition failed).
  `store.ErrGameNotFound` is reserved exclusively for a genuinely missing row —
  `UpdatePlayerBlack` no longer returns it under any circumstance, since the row's
  existence is already established by `JoinGame`'s prior `GetGame` call in every
  real call path.
- `Manager.JoinGame` gained an `errors.Is(err, store.ErrGameNotJoinable)` branch
  translating to `game.ErrGameNotJoinable` before the generic error-wrapping fallback.
- `JoinGame`'s pre-flight `GetGame` + `PlayerBlackID != nil` check (added as the first,
  incomplete fix) is retained. It is now purely a fast-path / friendly-error optimization
  for the non-racing case — the atomic UPDATE is the sole correctness guarantee. Removing
  the pre-flight check would not reintroduce the bug, but would lose the distinct
  `ErrSelfPlay` vs `ErrGameNotJoinable` error specificity in the common case.
- New regression test `TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins` in
  `internal/game/manager_race_test.go`: 20 trials per run, each spinning up two real
  goroutines racing `JoinGame` against a freshly created game, asserting exactly one
  success and one `ErrGameNotJoinable` failure, and cross-checking the persisted DB row
  against the in-memory `GameSession` to catch a bug that might otherwise only be
  visible at the SQL level. Passes under `-race`. This test does not and cannot catch
  the class of bug it guards against via the Go race detector alone — the underlying
  race is a database-level TOCTOU, not a Go memory race — so its value is in exercising
  genuine goroutine concurrency against real PostgreSQL, not in `-race` flagging
  anything directly.
- This is the second race condition found and fixed this session via direct code
  review and adversarial test-writing (the first being ADR-014's `ProcessMove`
  TOCTOU) rather than via a pre-existing spec requirement — both are now considered
  precedent for treating "was this checked for concurrent callers, not just sequential
  ones" as a standing question for any new read-then-write sequence added to the
  `game` or `store` layers, not just a one-off fix.

---

## ADR-017: HandleConnect Double-Join Race — Atomic State Transition

**Date:** 2026-07-01
**Status:** ACCEPTED

**Context:**
`HandleConnect`'s first-connect path has a Go memory race if both players connect concurrently. The sequence executes three distinct methods on `GameSession`:
1. `session.RegisterConnection(color, conn)`
2. `session.BothPlayersConnected()`
3. `session.Transition(store.GameStatusActive)`

Each method acquires and releases `session.mu` independently. If White and Black connect simultaneously, both goroutines can complete `RegisterConnection` before either evaluates `BothPlayersConnected`. Both will observe `true`, and both will call `Transition(ACTIVE)`. 

`Transition` correctly allows only one caller to succeed. The losing goroutine receives an `ErrInvalidTransition` and errors out of `HandleConnect`. This silently drops the losing player's WebSocket connection while leaving their `*ws.Connection` pointer dangling in the `GameSession`. Furthermore, it breaks the WebSocket message protocol: the winning goroutine sends `OPPONENT_CONNECTED` to a connection that is actively being dropped by the losing goroutine.

**Options Considered:**

**Option A: Atomic compound method on GameSession (CHOSEN)**
Combine the registration, presence check, and state transition into a single atomic operation inside `GameSession` under one lock acquisition. The method returns a boolean indicating if this specific call triggered the transition to `ACTIVE`.

- Pros: Eliminates the TOCTOU window completely. Ensures exactly one goroutine executes the downstream side-effects (DB persistence, clock start, opponent notification). Cleanest encapsulation of the state machine.
- Cons: Slightly couples connection registration with state machine progression inside `GameSession`.

**Option B: Catch `ErrInvalidTransition` in `HandleConnect`**
Let the race happen, catch the error on the losing goroutine, and treat it as a reconnect.

- Pros: No changes to `GameSession` API.
- Cons: Causes chaotic message broadcasting. The winning goroutine sends `OPPONENT_CONNECTED`, while the losing goroutine (treating it as a reconnect) sends `OPPONENT_RECONNECTED`. Clients receive contradictory state messages. Patches a symptom instead of fixing the underlying atomicity violation.

**Option C: Single-writer-per-session (Actor Model)**
Route all mutations through a single goroutine per session via channels (as discussed in ADR-014).

- Pros: Solves all intra-session concurrency races by construction.
- Cons: High implementation cost. Complicates synchronous error returns for connection rejection. Over-engineering for a localized race that a 5-line mutex fix can solve.

**Decision:** Option A — Atomic compound method on `GameSession`.

**Rationale:**
The race requires that connection registration and state transition occur atomically. Option A pushes this atomicity into `GameSession`, mirroring the fix from ADR-016 where the state-holding layer (the database) was made responsible for the atomic check-then-act. Option B is rejected because it corrupts the WebSocket message protocol with duplicate messages. Option C remains tabled as over-engineering for the current phase.

**Consequences:**
- `GameSession` will expose an atomic method (e.g., `RegisterAndMaybeActivate`) or `RegisterConnection` will be modified to return an `activated bool`.
- `HandleConnect` will use this boolean to ensure only one goroutine handles the transition side-effects (starting the clock, updating the DB, and broadcasting `OPPONENT_CONNECTED`).
- The losing concurrent caller will simply return `nil` and send a `GAME_STATE` message to itself, completing its connection lifecycle correctly.

**Implementation follow-up (2026-07-02):** `RegisterConnection`'s WAITING to ACTIVE branch was found, during code review, to assign `s.status` directly rather than routing through `Transition`/`validTransitions` -- a second, duplicated source of truth for legal state-machine edges. Refactored to extract `transitionLocked(newStatus) error` (no locking, assumes caller holds `s.mu`), called by both `Transition` (which locks) and `RegisterConnection` (already holding the lock). `validTransitions` is once again the single point where legal edges are decided. Added `TestRegisterConnection_ConcurrentBothConnect_ExactlyOneActivates` (200 trials, real goroutines, run under `-race`) per CLAUDE.md Non-Negotiable Constraint #10 -- the existing tests on `RegisterConnection` were all sequential and did not prove the race was closed. Both changes verified passing. No new ADR opened for this -- treated as an implementation-decision-level correction to ADR-017's own fix.

---

## ADR-018: ReadLoop / HandleMessage Context Lifetime

Date: 2026-07-02
Status: ACCEPTED

Context: ws.Connection.ReadLoop runs for the lifetime of a WebSocket connection, which outlives the HTTP request that established it -- ServeHTTP returns immediately after the upgrade and goroutine spawn, but ReadLoop (and the onMessage/onClose callbacks it drives) continue running until the connection closes. Per CODING_GUIDELINES.md section 2, every I/O function takes context.Context as its first argument -- this applies to Manager.HandleMessage and, in spirit, to the disconnect path as well. But there is no live request-scoped context available at the point onMessage/onClose fire, since r.Context() from the original upgrade request is already cancelled. This was flagged as a known sharp edge as far back as Step 7/8 (see the NOTE comment already present in ws/connection.go's ReadLoop doc) and called out repeatedly in CLAUDE.md as a decision that must be made deliberately before Step 11 implementation begins, not discovered mid-implementation.

Options Considered:

Option A: context.Background() per call. Each onMessage/onClose callback passes context.Background() directly to Manager.HandleMessage/HandleDisconnect. Pros: simplest possible implementation, no new state threaded through Handler. Cons: no cancellation ever propagates into the game layer from the server's lifecycle -- directly conflicts with PHASE_1.md Step 13's requirement to "wait for in-progress moves to complete" on SIGTERM, since there would be no context to cancel and therefore nothing for in-flight DB calls or the eventual graceful-shutdown wait to key off of.

Option B: Server-lifetime context, created at Handler construction, cancelled on SIGTERM (CHOSEN). Handler holds a context.Context set once at construction time. main.go (Step 13) creates it via context.WithCancel(context.Background()), passes the ctx into Handler's constructor, and calls the matching cancel() from its SIGTERM branch. Every ReadLoop callback, for every connection, for the life of the process, uses this same context. Pros: gives Step 13's graceful-shutdown requirement an actual mechanism to key off of -- shutdown cancels the context, in-flight store calls observe ctx.Err() on their next check, and main.go can bound the wait with the same waitWithTimeout pattern already used in ws/registry.go's CloseAll. Matches the scope of the thing being cancelled: the WebSocket session's lifetime is intentionally decoupled from any single HTTP request, so a server-lifetime context is the correctly-scoped choice, not a request-scoped one. Cons: Handler now holds a context.Context as a struct field, which is normally a code smell (CODING_GUIDELINES.md section 2 -- "never store context in a struct" -- is written with request-scoped contexts in mind). This is an intentional, documented exception: the guideline's rationale ("context belongs to the call, not the struct") assumes a per-call context is available; here there is none by construction, and the alternative (Option A) forfeits Step 13's shutdown requirement entirely. This exception is scoped narrowly to Handler's server-lifetime context field and must not be used as precedent for storing request-scoped contexts elsewhere.

Option C: Per-connection derived context, cancelled when that connection's ReadLoop exits. Each connection gets its own context.WithCancel(serverCtx), cancelled in ReadLoop's deferred cleanup. Pros: superficially appealing -- "scope cancellation to the connection." Cons: backwards. ReadLoop's deferred cleanup is exactly the code that calls onClose -> Manager.HandleDisconnect, which needs to run to completion (persist clock pause state, start the abandonment timer) -- racing that against its own about-to-be-cancelled context solves a problem that doesn't exist while creating one (a HandleDisconnect implementation that later adds a context-cancellation check could spuriously abort its own cleanup).

Decision: Option B -- server-lifetime context, owned by main.go, injected into Handler at construction.

Rationale: Step 13 explicitly requires waiting for in-progress moves before shutdown completes; a server-lifetime cancellable context is the only option of the three that gives that requirement a mechanism at all. The CODING_GUIDELINES.md section 2 "never store context in a struct" rule is written for the ordinary case where a per-call context is available and correctly threading it through call chains is possible; a long-lived WebSocket connection's message-handling callbacks are the documented exception to that ordinary case -- there is no call boundary to derive a context from, only a connection lifetime and a server lifetime, and the server lifetime is the correct scope.

Consequences:
- Handler's constructor takes context.Context as an explicit parameter/field, not context.Background() inline.
- cmd/server/main.go (Step 13, not yet implemented) must create this context via context.WithCancel, pass ctx into Handler's constructor, and call the matching cancel() in its SIGTERM branch, ordered before ws.Registry.CloseAll() so that in-flight HandleMessage calls observe cancellation before connections are force-closed.
- No change to any already-implemented layer -- this affects only internal/ws/handler.go (Step 11, in progress) and wiring not yet written (Step 13).
- This is a narrow, documented exception to CODING_GUIDELINES.md section 2 and must be called out as such at the Handler struct's field declaration, not left implicit.

---

## ADR-019: Detached-Context Pattern for HandleDisconnect's Clock-Persist Write

**Date:** 2026-07-09
**Status:** ACCEPTED

**Context:**

`Manager.HandleDisconnect` was extended this session to persist the paused clock reading to the database at the moment of disconnect, closing a real gap against PHASE_1.md acceptance criterion #3 (a player who disconnects mid-turn and is then caught by a hard `kill -9`, with no graceful shutdown ever running, would otherwise resume with time that was never actually theirs). Per CODING_GUIDELINES.md section 2, `HandleDisconnect` now takes `ctx context.Context` as its first argument, since it performs I/O.

The natural choice — using that `ctx` parameter directly for the new `gameStore.UpdateClocks` call — fails on **100% of graceful shutdowns**, not as a rare race. ADR-018 established that `WSHandler` holds a server-lifetime context (the same `ctx` threaded into every `onClose` callback, and therefore into every `HandleDisconnect` call), and `cmd/server/main.go`'s Step 13 shutdown sequence cancels that context *before* calling `ws.Registry.CloseAll()` — this ordering is itself required by ADR-018, so that in-flight `HandleMessage` calls observe cancellation before their connections are force-closed. But `CloseAll()` is exactly what triggers `HandleDisconnect` for every still-connected player. By the time any of those `HandleDisconnect` calls reach the new clock-persist code, `ctx` is already cancelled — so `gameStore.UpdateClocks(ctx, ...)` fails immediately with `"context canceled"` for every player still connected at shutdown time. This was not caught by `go test -race -tags integration`; it was caught by real E2E testing (PHASE_1.md Step 14, a real process, real `Ctrl+C`) during this session, confirmed reproducible on every run, not intermittent.

**Options Considered:**

**Option A: Reorder shutdown — call `ws.Registry.CloseAll()` before cancelling the server-lifetime context.**
Swap the order of steps 2 and 3 in `main.go`'s `shutdown()` sequence so connections are force-closed (and their disconnect-driven clock persists run) while `ctx` is still live, then cancel afterward.
- Pros: `HandleDisconnect`'s persist call can use `ctx` directly, no new pattern introduced.
- Cons: Directly reopens the exact race ADR-018 was written to close on the `HandleMessage` side. If a connection is mid-`HandleMessage` (e.g. processing a `MOVE`) when `CloseAll()` force-closes it, that goroutine's context would still be live and its I/O would continue racing against a connection that has already been torn down. Fixing one gap by reopening a previously-closed one is not a fix.

**Option B: Detach the clock-persist write from the parent context's cancellation, bound it with its own short timeout (CHOSEN).**
Use `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)` for the `UpdateClocks` call specifically, leaving `ctx` itself, and every other use of it in `HandleDisconnect`/`HandleMessage`, untouched.
- Pros: Does not touch ADR-018's shutdown ordering at all. The detached context is still bounded (5s), so a genuinely stuck DB call cannot hang shutdown indefinitely; it is not an unbounded escape hatch. Scoped to exactly the one call site that needs it.
- Cons: `context.WithoutCancel` is a pattern that, if applied carelessly elsewhere, could silently defeat cancellation propagation the codebase otherwise relies on (CODING_GUIDELINES.md section 2, Non-Negotiable Constraint #7). Requires being called out explicitly at the call site — a future reader skimming `HandleDisconnect` could reasonably assume `ctx` is used directly, as it is everywhere else in the function.

**Option C: Give the clock-persist write its own dedicated context, independent of the WSHandler-supplied `ctx` entirely (e.g. `context.Background()` with a timeout).**
- Pros: Superficially simpler — no `WithoutCancel` unwrapping, just a fresh context.
- Cons: Functionally identical to Option B in cancellation behavior, but loses whatever request-scoped values might one day be attached to `ctx` and would otherwise want to survive detachment. `context.WithoutCancel(ctx)` preserves values while only stripping cancellation and deadline — the more precise tool for "detach from cancellation, keep everything else." Option C achieves the same cancellation-detachment with a blunter instrument for no benefit at Phase 1's current feature set, but with a real (if currently latent) cost if request-scoped values are ever added to this context in a later phase.

**Decision:** Option B — `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`, scoped to `HandleDisconnect`'s clock-persist write only.

**Rationale:**

The correctness requirement is narrow: this one write must survive the exact cancellation-timing sequence ADR-018 deliberately created for an unrelated, already-correct reason (protecting in-flight `HandleMessage` calls). Reordering shutdown (Option A) fixes this gap by reopening a different, previously-solved one — an unacceptable trade. Detaching only this call from cancellation (Option B) while bounding it with its own timeout gives the write a real chance to complete during shutdown without weakening ADR-018's guarantee anywhere else. Option C achieves the same practical effect with a less precise primitive and no offsetting advantage.

This decision also generalizes into Non-Negotiable Constraint #12 (CLAUDE.md): a context's cancellation timing, correctly chosen for one operation, must be re-verified — not assumed safe by inheritance — for every other operation later wired through the same context. `HandleDisconnect` already took a `ctx` parameter for an unrelated reason (satisfying CODING_GUIDELINES.md section 2 once it started doing I/O), and the new clock-persist code was added to that existing parameter without re-examining what actually cancels it or when — exactly the failure mode Constraint #12 now requires checking for explicitly.

**Consequences:**
- `Manager.HandleDisconnect`'s clock-persist block uses `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`, not `ctx` directly, for the `gameStore.UpdateClocks` call. Called out explicitly in a comment at the call site and in CLAUDE.md's Known Sharp Edges.
- No change to `main.go`'s shutdown ordering — ADR-018's Consequences (cancel before `CloseAll()`) remain exactly as specified.
- `Manager.PersistActiveClockState` (the separate, explicit shutdown-time flush called from `main.go`'s `shutdown()` step 4) is unaffected — it already receives `shutdownCtx`, a freshly-created context with its own timeout, not the ADR-018 server-lifetime context, so it was never subject to this bug.
- This is the third instance this project has hit of "a context's cancellation semantics, correct in isolation, produce a bug once a second concern is wired through the same context" — ADR-014 and ADR-018 itself are the other two. Treated as a recurring category of bug worth a standing constraint (#12), not a one-off patch.

---

## ADR-020: GameSession.CloseConnections Same-Goroutine Ordering Requirement

**Date:** 2026-07-09
**Status:** ACCEPTED

**Context:**

Manual E2E testing (PHASE_1.md Step 14) found that WebSocket connections were never closed after a game ended — observed directly via resignation testing: both players' `wscat` sessions stayed open indefinitely after receiving `GAME_OVER`, and any further message sent by a client produced silent non-responses (`Manager.HandleMessage`'s registry lookup fails once `finalizeGame` has unregistered the session, and the resulting error was only logged, never reported back to the client). `Manager.finalizeGame` — the single centralized cleanup function called from every terminal-state path — never touched connections at all; it only cancels abandonment timers and unregisters the session from `GameRegistry`.

The naive fix — close both players' connections from inside `finalizeGame`, alongside the rest of its cleanup — was drafted first and rejected before implementation, after reading `eventbus.go` directly rather than assuming synchronous-like behavior from `LocalEventBus`. `finalizeGame` is called synchronously from the same goroutine that is about to publish (or has just published) `GAME_OVER` via `Manager.publishGameOver`, which calls `m.eventBus.Publish(ctx, GameEvent{...})`. `LocalEventBus.Publish`'s buffered-channel send (buffer size 8) only guarantees the event was *enqueued* for the subscriber goroutine (`Manager.startEventSubscriber`'s loop) to eventually pick up and forward to the players — it does not guarantee that goroutine has run yet by the time `Publish` returns. If `finalizeGame` closed connections immediately after `publishGameOver` returns, the close frame (written through the same connection's single-writer outbound queue) could reach the wire before the subscriber goroutine gets scheduled and forwards `GAME_OVER` through that same queue — a real, not theoretical, chance of the client receiving a close frame and never receiving `GAME_OVER` at all.

**Options Considered:**

**Option A: Close connections from `finalizeGame`, centralizing all terminal-state cleanup in one place.**
- Pros: Single call site for all terminal-state bookkeeping.
- Cons: Races the close frame against `GAME_OVER`'s own delivery, as described above. This is not a corner case reachable only under load — it is a structural race present on every single game completion, since `finalizeGame` and the `EventBus` subscriber's forwarding of `GAME_OVER` are always on different goroutines with no ordering guarantee between them via the buffered channel alone. Rejected as incorrect, not merely suboptimal.

**Option B: Close connections from the same goroutine, immediately after, whichever call actually sent GAME_OVER (CHOSEN).**
Two call sites, both already goroutines that perform the `GAME_OVER` send itself: `Manager.startEventSubscriber`'s loop (the common path), and `Manager.publishGameOver`'s `EventBus`-failure fallback branch (the degraded path, taken only if `eventBus.Publish` itself returns an error).
- Pros: Relies on nothing but Go's program-order guarantee within a single goroutine, plus the outbound queue's FIFO draining by that connection's single `WriteLoop` — both guarantees already relied upon elsewhere in this codebase (CODING_GUIDELINES.md section 3), not a new assumption. Same-goroutine, immediately-after ordering is the only mechanism in this design that actually guarantees `GAME_OVER` reaches the wire before the close frame.
- Cons: Two call sites instead of one, both of which must independently remember to call `CloseConnections` and must never be "simplified" back into `finalizeGame`. Requires an explicit warning comment at `finalizeGame` itself to prevent a future session from reintroducing Option A as a well-intentioned refactor.

**Option C: Make `LocalEventBus.Publish` synchronous — block until the subscriber goroutine has actually processed the event before returning.**
- Pros: Would make `finalizeGame`-based centralization (Option A) safe again.
- Cons: Changes `EventBus`'s fundamental contract (ADR-010 — fire-and-forget, buffered, decoupled from subscriber scheduling) for every event type, not just `GAME_OVER`, to fix a problem specific to one event type's cleanup ordering. Also directly conflicts with ROADMAP.md's Phase 2 seam: `RedisEventBus` is pub/sub over the network — "synchronous until the subscriber has processed it" is not a property Redis pub/sub can offer without inventing an acknowledgment protocol on top of it. Rejected as solving a narrow problem by weakening a broader, deliberately-chosen abstraction (ADR-010).

**Decision:** Option B — `GameSession.CloseConnections` called only from the same goroutine as, and immediately after, whichever call sent the terminal `GAME_OVER` message; never from `finalizeGame`.

**Rationale:**

The actual guarantee needed — "the close frame must not precede `GAME_OVER` on the wire" — has exactly one mechanism available in the current architecture that provides it for free: same-goroutine program order. `LocalEventBus`'s buffered-channel handoff (ADR-010's chosen design) does not provide a delivery guarantee, by design, and changing that (Option C) would be a disproportionate fix that also collides with the Phase 2 Redis seam. Reordering within `finalizeGame` cannot help either, since the problem is which goroutine performs the close, not when within a single goroutine's sequence it happens. Option B is the only one of the three that produces the guarantee using mechanisms already trusted elsewhere in the codebase, at the cost of two call sites instead of one.

This is now generalized as Non-Negotiable Constraint #13 (CLAUDE.md): a message-delivery-then-connection-close sequence must happen in the same goroutine that performed the delivery, in program order — never split across goroutines relying on a queue/channel's "accepted" signal as a proxy for "processed."

**Consequences:**
- `GameSession.CloseConnections(statusCode int, reason string)` added, with a doc comment stating explicitly which two call sites are correct and why `finalizeGame` must never be one of them.
- `Manager.finalizeGame` gained a doc comment warning against adding connection-closing logic there, specifically to prevent a future "simplification" from reintroducing this race.
- `Manager.startEventSubscriber`'s `GAME_OVER` branch and `Manager.publishGameOver`'s `EventBus`-failure fallback branch both now call `session.CloseConnections(wsCloseNormal, "game ended")` immediately after their respective `GAME_OVER` sends.
- Regression test `TestWSHandler_GameOver_ClosesConnectionsAfterDelivery` (`internal/api/ws_handler_test.go`) uses real dialed WebSocket connections specifically to prove both that connections close **and** that `GAME_OVER` is strictly delivered before the close frame for both players — the second assertion is what actually exercises this fix; a version of the fix using Option A would still pass a weaker test that only checked eventual closure.
- No change to `EventBus`'s interface or `LocalEventBus`'s buffered-channel semantics (ADR-010 stands unmodified) — Option C was rejected specifically to avoid this.

---