## PART 1 — SESSION SUMMARY

## Session Summary — Clock Implementation and Manager Orchestrator Complete

### What Was Built

**`internal/game/clock.go`** — full `Clock` implementation: channel-based design with `resetCh` (buffered(1)) carrying timer reconfigurations to a single background goroutine (`run()`), and `stopCh` for clean termination. Fields: `mu sync.Mutex`, `active store.Color`, `whiteRemaining`/`blackRemaining time.Duration`, `startedAt time.Time`, `paused`/`started bool`, `onTimeout func(store.Color)`, `stopCh`, `resetCh`. Public API: `NewClock(initialMs)`, `NewClockWithTimes(whiteMs, blackMs)` (for restore-from-DB with divergent remaining times), `SetTimeoutCallback`, `Start`, `Switch`, `Pause`, `Resume`, `TimeRemaining`, `Stop`, `IsStarted`. The `run()` goroutine never acquires `c.mu` — all communication with it is via channels to avoid lock-ordering issues with the timeout callback.

**`internal/game/clock_test.go`** — 9 unit tests, all passing with `-race`: initial time, countdown-to-timeout, switch changes active color, pause/resume preserves remaining time, Stop terminates goroutine cleanly, Stop is idempotent, Start is idempotent, Resume with mismatched color does not corrupt `active`, concurrent `TimeRemaining` reads are race-free. All goroutine-spawning tests use `goleak.VerifyNone(t)` with `c.Stop()` deferred after it (LIFO: Stop runs before goleak inspects).

**`internal/game/session.go`** — added `clock *Clock` field (documented as protected by its own internal mutex, not `session.mu`). Added `NewGameSessionFromDB(game *store.Game, board *notnil.Game) *GameSession` — hydrates a session from a persisted DB record plus a replayed board, used exclusively by `RestoreActiveGames`. Clock is constructed via `NewClockWithTimes` from persisted `WhiteTimeMs`/`BlackTimeMs` and is NOT started until both players reconnect.

**`internal/game/move.go`** — wired `session.clock.Switch()` into `ProcessMove` immediately after `ApplyMove` succeeds. Post-switch clock values are read via `TimeRemaining`, written to `session.UpdateClocks` (in-memory) and persisted via `gameStore.UpdateClocks` (non-fatal on failure — in-memory is authoritative). `handleGameOver` now calls `session.clock.Stop()` before transitioning to COMPLETED, preventing a stale timeout callback from firing on a finished game. `publishMoveApplied` signature changed from taking a `GameStateSnapshot` to taking explicit `whiteTimeMs, blackTimeMs int64` — removes the dependency on stale snapshot data.

**`internal/game/manager.go`** (new, Step 10) — full `Manager` orchestrator:
- `NewManager(...)` — depends on `GameRegistry`, `MoveProcessor`, `GameStore`, `MoveStore`, `EventBus`, JWT secret, `chess.Validator`
- `CreateGame(ctx, userID)` — generates UUID v7 game ID, persists, signs White's token, registers session, subscribes to EventBus
- `JoinGame(ctx, gameID, userID)` — validates `WAITING_FOR_PLAYER` status and rejects self-play, persists Black, signs Black's token
- `HandleConnect(ctx, gameID, color, conn)` — handles first-connect (transitions WAITING→ACTIVE, starts clock when both present), reconnect (cancels abandonment timer, resumes or starts clock), and post-restart reconnect (clock never started in this process, so `Start` instead of `Resume`)
- `HandleDisconnect(gameID, color)` — clears connection slot, pauses clock, starts 60s abandonment timer
- `HandleMessage(ctx, gameID, color, raw)` — routes MOVE/RESIGN/PING/unknown
- `RestoreActiveGames(ctx)` — loads all ACTIVE/WAITING games, replays moves via `GameFromMoves` (never trusts `current_fen`), detects and corrects zombie ACTIVE games (board already terminal but DB still ACTIVE) by persisting the correct COMPLETED status and skipping registry insertion
- `handleResign`, `handleTimeout`, `onAbandonTimeout` — each stops the clock, transitions state, persists outcome, publishes GAME_OVER
- `startEventSubscriber` — self-terminating goroutine per game: ranges over the EventBus channel, fans out to both players, exits and unsubscribes upon seeing a GAME_OVER event
- UUID v7 (`github.com/google/uuid`) used for game IDs — correct choice for a DB primary key (time-ordered, avoids B-tree index fragmentation from random inserts)

**`internal/game/errors.go`** — added `ErrGameNotJoinable`, `ErrSelfPlay` sentinels for `JoinGame`.

All changes verified with `go test -race ./internal/game/...` — full suite passing, including all Step 6–10 unit tests.

### Decisions Made

**Clock uses channel-based reconfiguration, not direct timer manipulation from arbitrary goroutines.** The background `run()` goroutine owns the `time.Timer` exclusively; all other methods (`Start`, `Switch`, `Pause`, `Resume`) compute new state under `c.mu` and send a `clockReset` message via `resetCh`. This avoids `c.mu` ever being held while blocked on a channel operation and keeps the goroutine the single writer of `timer`/`timerC`. No ADR needed — implementation detail within the established mutex-documentation pattern.

**`startedAt time.Time` added to the Clock spec.** The original CLAUDE.md spec omitted this field. Without it, `Switch()` and `Pause()` have no reference point to compute elapsed time since the last start. Flagged and corrected in CLAUDE.md before implementation began (per explicit instruction from previous session-end review).

**`Resume(color)` restarts `c.active`, not the passed `color`.** `Pause()` does not clear `active` — it remains the paused player's color. If `Resume` is called with a mismatched color (caller bug), the implementation logs an Error but does not override `active`, preventing silent corruption of clock state. Verified by `TestClock_Resume_WrongColor_LogsErrorAndResumesActive`.

**`RestoreActiveGames` reconstructs boards via `GameFromMoves`, never `GameFromFEN(current_fen)`.** This was flagged as a required Step 10 behavior in the prior session (Concern #3a) due to Step 8's non-fatal `UpdateCurrentFEN` failure mode, which can leave `current_fen` stale. `GameFromMoves` replays the authoritative `moves` table and is always correct.

**Zombie ACTIVE games are detected and corrected during restore, not left for a future job.** If `handleGameOver`'s `UpdateGameStatus` call previously failed (DB still shows ACTIVE despite an in-memory COMPLETED transition having already occurred and been published), `RestoreActiveGames` calls `DetectOutcome` on the replayed board. If the game is already over, the DB record is corrected to COMPLETED and the session is excluded from the registry — it is never added as a live, joinable game.

**UUID v7 for game IDs (DB primary key); UUID v4 reserved for non-DB identifiers (e.g. request IDs at Step 12).** Raised by the user mid-session. UUID v4 is fully random and causes B-tree index fragmentation on insert-heavy primary keys; UUID v7 is time-ordered and inserts append-only into the index. `github.com/google/uuid` replaced an initial hand-rolled `crypto/rand`-based UUID v4 generator that had been written without first checking for an existing, correct library — this was a process violation (the project's working agreement requires checking for existing libraries before writing custom logic) and was corrected immediately within the same session.

**Decision: no shared UUID helper function.** Game IDs and request IDs are different types living in different packages with different version requirements (v7 for game IDs in `internal/game`, v4 for request IDs in `internal/api` at Step 12). Both are one-line library calls. A shared helper would add indirection without benefit — call `uuid.NewV7()` / `uuid.New()` directly at each site.

**EventBus subscriber goroutine is self-terminating on GAME_OVER.** Rather than the Manager separately managing subscriber lifecycle and game lifecycle, the subscriber goroutine itself recognizes the terminal event type, calls the deferred `unsubscribe()`, and returns. This keeps goroutine cleanup colocated with the condition that makes the goroutine's continued existence unnecessary, and avoids the Manager needing to track per-game subscriber state separately from the `GameRegistry`.

**Clock timeout callback boundary race is accepted as TD-002, not engineered around.** A move arriving at `ProcessMove` at the exact instant a clock timeout fires can race with `handleTimeout`. Both paths call `session.Transition`, which is idempotent-safe (the loser of the race gets an error and no-ops, logged at Debug). This is documented inline in `handleTimeout` and is an accepted Phase 1 simplification — Phase 4 will introduce move-submission timestamps to resolve it properly.

### Tradeoffs Considered

**Per-player abandonment timers via `time.AfterFunc` vs. a single sweep goroutine.** A periodic sweep goroutine scanning all sessions for disconnected players past the abandonment window was considered. `time.AfterFunc` per disconnect event was chosen: it is simpler, requires no polling interval tuning, and Go's runtime timer implementation handles many concurrent timers efficiently. The cost — a `map[string]*time.Timer` requiring its own mutex — is small and already follows the established mutex-documentation pattern.

**`HandleConnect` distinguishing first-connect from reconnect via pre-registration status snapshot vs. a separate "hasEverConnected" flag.** Snapshotting `session.CurrentStateSnapshot().Status` before calling `RegisterConnection` was chosen over adding new session state, because `WAITING_FOR_PLAYER` vs `ACTIVE` already encodes exactly this distinction without introducing a new field to keep in sync.

**Manual UUID v4 generation (initial draft) vs. `google/uuid` library.** Initial implementation used `crypto/rand` directly with manual version/variant bit-setting. Rejected upon review: bug-prone (easy to mis-set bits), non-standard, and unnecessary when a well-tested library is one `go get` away. This was a clear case of the project's "use libraries, don't reinvent" principle (consistent with ADR-006's rationale for using `notnil/chess` instead of writing a chess engine) being violated and then corrected.

### Lessons Learned

**A spec gap (missing `startedAt` field) surfaces immediately once you try to write `Switch()`/`Pause()` — but only if you actually attempt to write the deduction logic rather than just declaring the struct.** This reinforces the value of writing the full method bodies before considering a struct design "locked," rather than treating field lists as complete in isolation.

**Library-first discipline needs to be enforced even for "trivial" utility code, not just domain logic.** The chess validation library (ADR-006) was an obvious case for using a library. UUID generation felt small enough to hand-roll, which is exactly the trap — small, "obviously correct" utility code is where custom implementations introduce subtle bugs (version/variant bit errors) that a library has already solved correctly and tested.

**The EventBus self-terminating-subscriber pattern works cleanly with the existing `LocalEventBus.Publish` RLock-for-entire-send-loop design from Step 7.** No friction was found integrating `startEventSubscriber`'s `return` (closing via `defer unsubscribe()`) with the documented Known Sharp Edge about not snapshotting-then-releasing in `Publish` — the two were designed independently in different sessions and composed correctly on the first attempt.

### Problems Encountered

**Initial `manager.go` write used `crypto/rand`-based manual UUID v4 generation instead of checking for an existing library first.** Caught by the user, not self-identified — this is the most significant process gap this session. Corrected immediately: added `github.com/google/uuid`, switched to `uuid.NewV7()`, removed the hand-rolled `generateGameID` function entirely.

**An `edit_file` MCP call failed with a parameter validation error (`newText` undefined) on a multi-edit batch.** Root cause: one edit's `oldText` did not exactly match file content due to a prior partial/failed edit application. Resolved by re-reading the full file and rewriting it via `write_file` instead of patching, which is the safer recovery path when `str_replace`/`edit_file` matches become unreliable mid-session.

No unresolved problems carry into the next session.

### Checklist Progress

- ✅ Step 9: Clock — `Clock` struct, full public API, channel-based goroutine, 9 unit tests passing with `-race` and `goleak`
- ✅ Step 9: `GameSession` wired with `clock *Clock` field
- ✅ Step 9: `ProcessMove` calls `clock.Switch()`, persists clock state, passes live values to `MOVE_APPLIED`
- ✅ Step 10: `Manager` struct and `NewManager` constructor
- ✅ Step 10: `CreateGame(ctx, userID) (*GameSession, string, error)`
- ✅ Step 10: `JoinGame(ctx, gameID, userID) (string, error)`
- ✅ Step 10: `HandleConnect(ctx, gameID, color, conn) error` — first-connect, reconnect, post-restart-reconnect paths
- ✅ Step 10: `HandleDisconnect(gameID, color)`
- ✅ Step 10: `HandleMessage(ctx, gameID, color, raw) error` — MOVE/RESIGN/PING/unknown routing
- ✅ Step 10: `RestoreActiveGames(ctx) error` — stale-FEN and zombie-ACTIVE handling
- ✅ Step 10: `MsgTypeMove → ProcessMove`, `MsgTypeResign → handleResign`, `MsgTypePing → PONG`, unknown → ERROR
- ✅ UUID v7 adopted for game ID generation via `github.com/google/uuid`
- 🔄 Step 11 (WebSocket Handler) — not started; next task

### Technical Debt Introduced

None new this session. TD-002 (clock pauses on disconnect) now has a corollary boundary-race condition between `ProcessMove`-driven game completion and `handleTimeout` — this is an extension of TD-002, not a new debt item, and is documented inline in `manager.go`'s `handleTimeout`.

### Files Modified

**Created:**
- `internal/game/clock.go`
- `internal/game/clock_test.go`
- `internal/game/manager.go`

**Modified:**
- `internal/game/session.go` — added `clock *Clock` field, `NewGameSessionFromDB` constructor
- `internal/game/move.go` — wired `clock.Switch()` into `ProcessMove`, `clock.Stop()` into `handleGameOver`, changed `publishMoveApplied` signature
- `internal/game/errors.go` — added `ErrGameNotJoinable`, `ErrSelfPlay`
- `go.mod` / `go.sum` — added `github.com/google/uuid`

### Recommended Next Step

**Step 11: WebSocket Handler — implement `internal/ws/handler.go`.**

Before writing code, resolve the context-lifetime decision flagged in Known Sharp Edges: `r.Context()` is cancelled when `ServeHTTP` returns, which happens before the read loop (and thus `HandleMessage` calls) exit. Decide whether to pass `context.Background()`, a context derived from the server's top-level lifecycle context (passed in at `Handler` construction), or another approach — then implement `Handler` struct (deps: `auth.TokenService`, `*game.Manager`, `*ws.Registry`), `ServeHTTP` (token extraction from `?token=`, verification, `claims.GameID` vs URL `:id` match check, upgrade, `ws.Registry.Register`, `Manager.HandleConnect`, wire `ReadLoop` callbacks to `Manager.HandleMessage`/`Manager.HandleDisconnect`), plus integration tests: invalid token → connection refused with correct close code, valid token → GAME_STATE received, reconnection with same token → current GAME_STATE delivered. Estimated 2-3 hours given the context-lifetime decision needs deliberate review before implementation, not just during it.

---

## PART 2 — UPDATED CLAUDE.md

```markdown
# CLAUDE.md — Session Context Document

This file is the authoritative context document for AI-assisted development sessions on this project.

**Read this first. Every session. Before writing any code.**

Update this file at the end of every session. Stale context is worse than no context.

---

## Project Identity

**Name:** chess-server
**Module path:** `github.com/vedant-2701/chess`
**Language:** Go 1.22+
**Type:** Learning project — production-grade chess platform, phase-by-phase
**Primary Goal:** Learn system design, distributed systems, real-time backend architecture
**NOT a goal:** Build a Chess.com competitor

---

## Current Phase

**Phase 1 — MVP**
**Status: 🔄 In Progress — Steps 1–10 Complete, Step 11 (WebSocket Handler) Next**

---

## Completed Work

### Documentation
- [x] Project purpose and scope defined
- [x] Full tech stack decided and rationale documented
- [x] All documentation files created
- [x] Phase 1 spec written (PHASE_1.md)
- [x] Architecture documented (ARCHITECTURE.md)
- [x] All ADRs logged (DECISIONS_LOG_PHASE_1.md)
- [x] Coding guidelines defined (CODING_GUIDELINES.md)
- [x] `turn` field casing corrected to uppercase (`"WHITE"`/`"BLACK"`) in CLAUDE.md — PHASE_1.md is authoritative
- [x] `GET /games/:id` added to ARCHITECTURE.md endpoint list
- [x] Clock spec corrected to include `startedAt time.Time` field and precise `Resume` semantics before Step 9 implementation began
- [x] Step 10 sharp edges (stale `current_fen`, zombie ACTIVE games) documented before Step 10 implementation began

### Implementation
- [x] WebSocket infrastructure (`internal/ws`): connection lifecycle, read loop (callback-based), write loop, heartbeats, registry, graceful shutdown — ported from learning project and updated for production use
- [x] Step 1: Project Scaffold — go.mod, docker-compose.yml, .env.example, Makefile, directory structure
- [x] Step 2: Database Migrations — 3 up/down pairs (users, games, moves), verified migrate-up/down/idempotency
- [x] Step 3: Store Layer — postgres.go, game_store.go, move_store.go, user_store.go + integration tests (all passing with -race)
- [x] Step 4: Auth Layer — token.go (SignPlayerToken, VerifyPlayerToken) + unit tests (all passing with -race)
- [x] Step 5: Chess Layer — errors.go, types.go, validator.go + 20 unit tests (all passing with -race)
- [x] Step 6: Game Session and Registry — session.go, registry.go, errors.go + unit tests (all passing with -race)
- [x] Step 7: EventBus — eventbus.go, messages.go + unit tests (all passing with -race)
- [x] Step 8: Move Pipeline — move.go (MoveProcessor, MoveRejectionError, ComputeFENAfterMove) + 5 integration tests (all passing with -race)
- [x] Step 9: Clock — clock.go (channel-based design, goroutine-per-clock), wired into session.go and move.go, clock_test.go with 9 unit tests passing with -race and goleak
- [x] Step 10: Manager — manager.go (CreateGame, JoinGame, HandleConnect, HandleDisconnect, HandleMessage, RestoreActiveGames, abandonment timers, resign/timeout/abandon game-over paths)

---

## Phase 1 Checklist

### Foundation
- [x] go.mod initialized with all dependencies
- [x] .env.example created
- [x] docker-compose.yml created (PostgreSQL + Redis placeholder)
- [x] Makefile created with standard targets
- [x] Directory structure scaffolded

### Database
- [x] Migration 001: create users table
- [x] Migration 002: create games table
- [x] Migration 003: create moves table
- [x] pgxpool connection setup (internal/store/postgres.go)
- [x] Store layer implemented (game_store.go, move_store.go, user_store.go)

### Auth Layer
- [x] JWT sign function (playerToken: gameID + userID + color)
- [x] JWT verify function
- [x] Anonymous userID generation (client-side UUID; server accepts via CreateOrGetUser upsert)

### Chess Layer
- [x] notnil/chess integration
- [x] Move validation without mutation (ValidateMove)
- [x] Move application after DB write (ApplyMove)
- [x] FEN after move without mutation (ComputeFENAfterMove)
- [x] Game result detection (DetectOutcome)
- [x] FEN extraction (CurrentFEN)
- [x] SAN move recording (MoveHistory)
- [x] GameFromFEN, GameFromMoves for state reconstruction

### Game Layer
- [x] GameSession struct defined
- [x] GameState machine (WAITING → ACTIVE → COMPLETED / ABANDONED)
- [x] GameRegistry (gameID → *GameSession)
- [x] EventBus interface defined (LocalEventBus for Phase 1)
- [x] Player-to-connection bridge for reconnection (session pointer slots + ReplaceConnection)
- [x] messages.go — single source of truth for all WebSocket protocol strings
- [x] MoveProcessor — full 8-step pipeline (Step 8), clock-switching wired in (Step 9)
- [x] MoveRejectionError — typed client-facing rejection error (Step 8)
- [x] Clock — per-game countdown timers, timeout detection, channel-based goroutine (Step 9)
- [x] Manager — top-level orchestrator: game creation/join, connection lifecycle, message routing, restart recovery (Step 10)

### API Layer (HTTP)
- [ ] chi router setup
- [ ] POST /games — create game, return gameID + white's playerToken
- [ ] POST /games/:id/join — join game, return black's playerToken
- [ ] GET /games/:id — get current game state (for reconnection via HTTP)
- [ ] GET /health — health check

### WebSocket Layer
- [x] ws infrastructure ported and updated (ReadLoop callback-based, TextMessage, slog)
- [ ] WS upgrade handler at GET /ws/game/:id
- [ ] Token validation on connect
- [ ] Player registration into GameSession on connect
- [ ] Message routing (MOVE, RESIGN, PING)

### Move Pipeline
- [x] Receive MOVE message from client
- [x] Validate it is the correct player's turn
- [x] Validate move legality via chess library
- [x] Persist move to database
- [x] Update current_fen on game record
- [x] Broadcast MOVE_APPLIED to both players (via EventBus)
- [x] Check for game over after each move
- [x] Reject illegal moves with MOVE_REJECTED response (typed error for Manager)

### Time Controls
- [x] Server-side clock per game (10 minutes per player)
- [x] Clock starts when both players are connected
- [x] Clock switches on each move
- [x] Timeout detection goroutine per game
- [x] GAME_OVER broadcast on timeout

### Reconnection
- [x] Player reconnects with playerToken — handled by Manager.HandleConnect (WebSocket-layer wiring deferred to Step 11)
- [x] Server maps token to existing GameSession — via GameRegistry.Get
- [x] Old connection pointer replaced with new connection — ReplaceConnection
- [x] Full game state sent to reconnecting player (GAME_STATE message) — sendGameState
- [x] Opponent notified of reconnection (OPPONENT_RECONNECTED message)

### Persistence Recovery
- [x] On server restart, active games are recoverable from DB — RestoreActiveGames
- [x] GameSession can be hydrated from DB records — NewGameSessionFromDB
- [x] In-progress games resume correctly after server restart — board via GameFromMoves, zombie ACTIVE games corrected

### Testing
- [x] Store layer: integration tests with real PostgreSQL (integration build tag)
- [x] Auth layer: unit tests (no build tag, no database)
- [x] Chess layer: unit tests (no build tag, no database)
- [x] Game session and registry: unit tests (no build tag, no database)
- [x] EventBus: unit tests (no build tag, no database)
- [x] Move pipeline: integration tests (integration build tag, real PostgreSQL)
- [x] Clock: unit tests with goleak goroutine-leak verification (no build tag, no database)
- [ ] Manager: integration tests (CreateGame, JoinGame, RestoreActiveGames with real PostgreSQL) — not yet written; should be added alongside or before Step 11 WebSocket handler tests
- [ ] WebSocket handler: httptest-based tests
- [ ] Reconnection scenario: integration test

---

## Architectural Decisions (Summary)

Full rationale in DECISIONS_LOG_PHASE_1.md. This is the quick-reference list.

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-001 | Language | Go 1.22+ |
| ADR-002 | WebSocket library | gorilla/websocket |
| ADR-003 | HTTP router | go-chi/chi v5 |
| ADR-004 | Database | PostgreSQL 16 |
| ADR-005 | DB driver | pgx/v5 (pgxpool) |
| ADR-006 | Chess library | notnil/chess |
| ADR-007 | MVP matchmaking strategy | Shared link (no matchmaking queue) |
| ADR-008 | Auth strategy | JWT player tokens (stateless, scoped per game) |
| ADR-009 | Registry architecture | Two separate registries: ws.Registry + game.GameRegistry |
| ADR-010 | Event bus | Interface with LocalEventBus in Phase 1, RedisEventBus in Phase 2 |
| ADR-011 | ORM strategy | No ORM. pgx/v5 with raw SQL |
| ADR-012 | Framework | No framework. chi for routing, stdlib everywhere else |
| ADR-013 | Chess move validation strategy | Validate-Then-Apply split (ValidateMove + ApplyMove separate methods) |

### Implementation Decisions (No ADR Required)

| Decision | Chosen | Rationale |
|----------|--------|-----------|
| `UpdateGameStatus` signature | `*GameOutcome` (not `*Outcome`) | outcome + reason always move together |
| Game UUID generation | App-generated (game layer), not DB DEFAULT | JWT must be signed before DB round-trip |
| Game ID UUID version | **UUID v7** via `github.com/google/uuid`, not v4 | Time-ordered IDs avoid B-tree index fragmentation on insert-heavy primary keys; v4 reserved for non-DB identifiers (e.g. request IDs, Step 12) |
| `scanGame` helper | `func(dest ...any) error` parameter | Both `pgx.Row.Scan` and `pgx.Rows.Scan` satisfy this |
| Nullable column scanning | `*string` intermediates, convert to typed pointer | Avoids pgx/v5 reflection path for user-defined string types |
| Store test package | `package store` (internal) | Too many exported domain types to prefix |
| `Color` in PlayerClaims | `string` (not `store.Color`) | Keeps `internal/auth` free of `internal/store` dependency |
| `GetMovesForGame` empty result | `make([]*Move, 0)` (non-nil) | Serializes to `[]` not `null` in JSON |
| `GetActiveGames` empty result | `make([]*Game, 0)` (non-nil) | Same reason |
| `GameOutcome` in `internal/chess` | Separate from `internal/store` types | Keeps chess layer free of store dependency |
| `MoveHistory` uses `g.Moves()` + `g.Positions()` | Not `g.MoveHistory()` | `g.MoveHistory()` panics on nil comments slice (library bug v1.9.0) |
| `DetectOutcome` default draw reason | `"DRAW_AGREEMENT"` | Only valid schema value for non-stalemate draws |
| `GameFromFEN` vs `GameFromMoves` for restart recovery | Both exposed; **RestoreActiveGames always uses GameFromMoves** | FEN is O(1) for reconnection display; moves required for accurate restart recovery since current_fen can be stale (see Known Sharp Edges) |
| `ReadLoop` signature | `ReadLoop(onMessage func([]byte), onClose func())` | ws layer stays ignorant of game layer; context lifetime deferred to Step 11 |
| `Send()` message type | `websocket.TextMessage` | Chess server sends JSON, not binary frames |
| `LocalEventBus.Publish` holds RLock during sends | Snapshot-then-release rejected | Snapshot creates window where unsubscribe can close channel mid-send — send-on-closed panic |
| `messages.go` in `internal/game` | No separate `internal/proto` package | ws layer is byte-transparent; only game layer needs protocol strings |
| `SendToPlayer` / `SendToBothPlayers` on `GameSession` | Not in PHASE_1.md spec but required | Move pipeline needs to push messages without exposing `*ws.Connection` outside session |
| `UpdateClocks` on `GameSession` | Added at Step 6 | Clock (Step 9) needs to write remaining time back to session; cleaner to define the method now |
| "Player-to-connection bridge" checklist item | Implemented via session pointer slots + `ReplaceConnection` | No separate data structure needed; Manager is the caller |
| `MoveRejectionError` typed error | Added at Step 8 | Manager uses `errors.As` to distinguish client rejections from infrastructure failures |
| `ComputeFENAfterMove` uses `Position.Update(*chess.Move).String()` | Not FEN clone | No game allocation; no mutation; uses same Decode path as ValidateMove |
| Single `mu.RLock()` for ValidateMove + ComputeFENAfterMove | Not two separate lock acquisitions | Position cannot change between the two reads; avoids two round-trips |
| `UpdateCurrentFEN` failure in ProcessMove | Non-fatal; logged, pipeline continues | move is in moves table (source of truth); current_fen is a cache — **but this means current_fen can go stale; RestoreActiveGames must never trust it (see Known Sharp Edges)** |
| `handleGameOver` publishes GAME_OVER even on DB failure | Continues after log | Players must be notified regardless of DB consistency — **but this can leave a DB record as ACTIVE when the game is actually COMPLETED; RestoreActiveGames detects and corrects this (zombie ACTIVE, see Known Sharp Edges)** |
| Clock struct includes `startedAt time.Time` | Added during Step 9 spec review, before implementation | Required to compute elapsed time for Switch/Pause deduction; omitted from initial CLAUDE.md draft |
| `Clock.Resume(color)` restarts `c.active`, not `color` | Mismatch logged as Error, `active` never overridden | Silently reassigning the active color on a caller bug would corrupt clock state |
| Clock background goroutine never acquires `c.mu` | All state changes communicated via `resetCh`/`stopCh` | Avoids lock-ordering issues between the goroutine and exported methods; goroutine is sole owner of `time.Timer` |
| `NewClockWithTimes(whiteMs, blackMs)` | Separate constructor from `NewClock(initialMs)` | RestoreActiveGames needs to hydrate a Clock with two independently-diverged remaining times from the DB |
| `RestoreActiveGames` board reconstruction | Always `GameFromMoves`, never `GameFromFEN(current_fen)` | current_fen can be stale per UpdateCurrentFEN non-fatal failure mode (TD-007 / Known Sharp Edges) |
| Zombie ACTIVE game handling | Detected via `DetectOutcome` on replayed board during restore; DB corrected, session excluded from registry | handleGameOver's GAME_OVER-on-DB-failure decision can leave DB inconsistent; restore must self-heal |
| EventBus subscriber goroutine lifecycle | Self-terminating: returns and calls `unsubscribe()` upon delivering a GAME_OVER event | Keeps cleanup colocated with the condition that ends the goroutine's usefulness; avoids separate subscriber-tracking state in Manager |
| Abandonment timer mechanism | Per-player `time.AfterFunc`, keyed by `gameID+":"+color`, tracked in `Manager.abandonTimers` map under `Manager.mu` | Simpler than a periodic sweep goroutine; Go runtime handles many concurrent timers efficiently |
| Game ID generation library | `github.com/google/uuid`, `uuid.NewV7()` | Initial implementation hand-rolled UUID v4 via crypto/rand — corrected: violates library-first principle (ADR-006 precedent), and v7 is structurally correct for a DB primary key |
| Request ID generation (planned, Step 12) | `uuid.New()` (v4) inline at the API layer, no shared helper | Different package, different version requirement from game IDs; a shared helper adds indirection without benefit for a one-line call |

---

## Technical Debt

```
TD-001: Player token passed in URL query parameter (visible in logs) | Phase 1 | Fix by: Phase 3
TD-002: Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 | Fix by: Phase 4
        Corollary (Step 10): boundary race between ProcessMove-driven completion
        and Clock timeout firing at the same instant. Both paths call
        session.Transition, which is idempotent-safe (loser no-ops, logged Debug).
        Not a new debt item — extension of TD-002's root cause (no move timestamps).
TD-003: No draw offer mechanism | Phase 1 | Fix by: Phase 4
TD-004: Anonymous identity only (no real user accounts) | Phase 1 | Fix by: Phase 3
TD-005: Single time control (10+0 only) | Phase 1 | Fix by: Phase 4
TD-006: DetectOutcome maps ThreefoldRepetition/FiftyMoveRule/InsufficientMaterial to "DRAW_AGREEMENT" | Phase 1 | Fix by: Phase 4
TD-007: GameFromFEN loses position history — threefold repetition blind after server restart | Phase 1 | Fix by: Phase 4
        Note: RestoreActiveGames (Step 10) uses GameFromMoves, not GameFromFEN, for
        exactly this reason — the restored board has full position history.
```

---

## Non-Negotiable Constraints

These decisions are locked. Do not revisit without a new ADR.

1. **Server is authoritative for all game state.** Client validation is for UX only.
2. **No client timers for time controls.** Server-side clock only.
3. **Every move is persisted before being broadcast.** Persistence is on the critical path.
4. **No Redis in Phase 1.** EventBus interface must be used so Phase 2 swap is clean.
5. **No ORM.** Raw SQL via pgx/v5 only.
6. **No global state.** All state passed via dependency injection.
7. **Every I/O function takes context.Context as first argument.**
8. **ValidateMove before DB write; ApplyMove after DB write succeeds.** Never mutate board state before persistence confirms. (ADR-013)
9. **Check for an existing, well-tested library before writing custom logic for solved problems** (e.g. UUID generation, chess move validation). Hand-rolling "trivial" utility code is exactly where subtle bugs hide.

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://` (for the migrate CLI in the Makefile). When migrations are wired into `main.go` at Step 13, use `pgx5://` scheme with the `golang-migrate/migrate/v4/database/pgx/v5` driver. Different URL schemes, same database — correct and intentional, but will cause confusion at Step 13 if not remembered.

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Any game constructed via `NewGame()` + `MoveStr()` has a nil `comments` field. Do not call `g.MoveHistory()` directly anywhere. Always use the `chess.MoveHistory(g)` wrapper in `internal/chess/validator.go`.

- **`MoveHistory()` returns annotated SAN.** `AlgebraicNotation.Encode` appends `+` for check and `#` for checkmate. The `"moves"` field in `GAME_STATE` messages will contain annotated SAN. Step 8 move pipeline sources move history from `chess.MoveHistory(session.board)` via `CurrentStateSnapshot()`, not from a cached copy of raw client input.

- **`DetectOutcome` must be called after `ApplyMove`.** Calling it before any move is played will always return `(nil, false)` even for checkmate/stalemate positions loaded from FEN.

- **`ReadLoop` context decision deferred to Step 11.** The HTTP request context (`r.Context()`) is cancelled when `ServeHTTP` returns, which is before `ReadLoop` exits. At Step 11, decide whether to pass `r.Context()`, a derived context, or `context.Background()` to `HandleMessage`. The comment in `ReadLoop` flags this explicitly. **This decision must be made deliberately before Step 11 implementation begins, not discovered mid-implementation.**

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** This is intentional. Do not "optimise" it to snapshot-then-release — that pattern creates a window where `unsubscribe` can close a channel between the snapshot and the send, causing a send-on-closed-channel panic.

- **`ComputeFENAfterMove` must only be called after `ValidateMove` returned nil for the same `(g, san)` pair.** Both use `AlgebraicNotation{}.Decode` internally. If `ValidateMove` passed, `ComputeFENAfterMove` will not error. If it does error, it is a bug — log at Error level and return a plain (non-rejection) error.

- **`move.go` accesses `session.board` and `session.mu` directly.** This is valid because `move.go` is in `package game` — same package as `session.go`. Private fields are accessible within a package. `mu.RLock()` is held for `ValidateMove` + `ComputeFENAfterMove`; `mu.Lock()` is held for `ApplyMove` only.

- **`Clock` struct requires a `startedAt time.Time` field — omitted from original Step 9 spec.** `Switch()` and `Pause()` deduct elapsed time since the clock last started from the active player's remaining duration, using `time.Since(startedAt)`. `startedAt` is set on every `Start`/`Resume` call. This is implemented correctly in the current `clock.go` — flagging here for any future reimplementation reference.

- **`Resume(color Color)` restarts the timer for `c.active`, not `color`.** `Pause()` does not clear `c.active`, so it always holds the paused player's color. If `color != c.active` on `Resume`, an Error is logged (caller bug) but `c.active` is NOT overridden. Implemented and tested (`TestClock_Resume_WrongColor_LogsErrorAndResumesActive`).

- **`Clock.run()` never acquires `c.mu`.** All exported methods compute new state under `c.mu`, then send a `clockReset` message via the buffered(1) `resetCh` channel to the background goroutine, which is the sole owner of the `time.Timer`. This avoids the goroutine ever blocking while holding `c.mu`, and avoids `c.mu` being held during a channel send that could theoretically block.

- **`RestoreActiveGames` must reconstruct boards via `chess.GameFromMoves`, never `chess.GameFromFEN(game.CurrentFEN)`.** `UpdateCurrentFEN`'s non-fatal failure mode (Step 8) means `games.current_fen` can be stale relative to the `moves` table, which is the source of truth. Implemented correctly in `manager.go`'s `restoreGame`.

- **`RestoreActiveGames` must detect and correct zombie ACTIVE games.** `handleGameOver`'s decision to publish GAME_OVER even when `UpdateGameStatus` fails (Step 8) can leave a game COMPLETED in memory (and already broadcast as such to players) but still recorded as ACTIVE in the DB. `GetActiveGames` will return it on the next restart. `restoreGame` calls `DetectOutcome` on the replayed board; if the game is already over, it corrects the DB status to COMPLETED and does NOT add the session to the registry. Implemented in `manager.go`.

- **Integration tests for `internal/game` use `//go:build integration`.** Running `go test -race ./internal/game/...` runs only unit tests. Running `go test -race -tags integration ./internal/game/...` runs both unit and integration tests (the `TestMain` in `testmain_integration_test.go` sets up the DB pool; unit tests ignore it). **Note: Manager does not yet have integration tests (CreateGame, JoinGame, RestoreActiveGames) — should be written alongside or before Step 11.**

- **`handleTimeout` must never call any `Clock` method.** It is invoked from within the Clock's own background goroutine (`run()`) via the registered `onTimeout` callback. Calling back into the Clock that is currently executing the callback would deadlock or corrupt state. `handleTimeout` only touches `GameSession` and store/EventBus — never `session.clock`.

---

## WebSocket Message Protocol (Phase 1)

All constants are defined in `internal/game/messages.go` — never hardcode these strings.

### Client → Server

```json
{ "type": "MOVE",   "san": "e4" }
{ "type": "RESIGN" }
{ "type": "PING" }
```

### Server → Client

```json
{ "type": "GAME_STATE",           "fen": "...", "turn": "WHITE", "moves": ["e4","e5"], "status": "ACTIVE", "whiteTimeMs": 598000, "blackTimeMs": 600000, "outcome": null, "outcomeReason": null }
{ "type": "MOVE_APPLIED",         "san": "e4", "fen": "...", "turn": "BLACK", "moveNumber": 1, "whiteTimeMs": 597843, "blackTimeMs": 600000 }
{ "type": "MOVE_REJECTED",        "san": "e5", "reason": "not your turn" }
{ "type": "GAME_OVER",            "outcome": "WHITE", "reason": "CHECKMATE", "fen": "..." }
{ "type": "OPPONENT_CONNECTED" }
{ "type": "OPPONENT_DISCONNECTED" }
{ "type": "OPPONENT_RECONNECTED" }
{ "type": "ERROR",                "code": "INVALID_TOKEN", "message": "..." }
{ "type": "PONG" }
```

`turn`, `outcome`, `color` values are always uppercase strings: `"WHITE"` / `"BLACK"` / `"DRAW"`.

**Note on `moves` field in GAME_STATE:** Contains annotated SAN from `chess.MoveHistory()`. Check moves include `+`, checkmates include `#`.

**Note on clock values in GAME_STATE and MOVE_APPLIED:** As of Step 9/10, all clock values sent to clients are live reads from `session.clock.TimeRemaining()`, accounting for in-flight elapsed time. No longer pre-move/stale snapshot values (that limitation, noted in earlier sessions, was resolved by Step 9's clock wiring).

---

## Key Files and Their Responsibilities

```
cmd/server/main.go            — Wires all dependencies, starts HTTP server, handles OS signals
internal/ws/connection.go     — Connection struct, read loop (callback-based), write loop, heartbeat
internal/ws/registry.go       — connID -> *Connection map with RWMutex
internal/ws/errors.go         — ErrConnectionClosed, ErrQueueFull
internal/ws/handler.go        — HTTP upgrade, token validation, GameManager handoff [Step 11]
internal/game/errors.go       — ErrGameNotFound, ErrConnectionOccupied, ErrInvalidTransition,
                                MoveRejectionError, ErrGameNotJoinable, ErrSelfPlay
internal/game/messages.go     — All WebSocket message type strings, rejection reasons, error codes
internal/game/session.go      — GameSession struct, state machine, connection management, snapshots,
                                clock field, NewGameSessionFromDB (restore-from-DB constructor)
internal/game/registry.go     — gameID -> *GameSession map with RWMutex
internal/game/eventbus.go     — EventBus interface + LocalEventBus implementation
internal/game/manager.go      — Top-level orchestrator: CreateGame, JoinGame, HandleConnect,
                                HandleDisconnect, HandleMessage, RestoreActiveGames,
                                handleResign/handleTimeout/onAbandonTimeout, abandonment timers
internal/game/move.go         — MoveProcessor: 8-step pipeline + clock switching (Step 9 wiring),
                                MoveRejectionError, handleGameOver (stops clock)
internal/game/clock.go        — Clock: channel-based per-game countdown timers, timeout detection
internal/chess/errors.go      — ErrIllegalMove, ErrInvalidFEN sentinels
internal/chess/types.go       — GameOutcome{Winner, Reason} — chess-layer result type
internal/chess/validator.go   — Validator, NewGame, GameFromFEN, GameFromMoves,
                                ValidateMove, ApplyMove, ComputeFENAfterMove,
                                DetectOutcome, CurrentFEN, MoveHistory
internal/store/errors.go      — ErrGameNotFound, ErrUserNotFound sentinels
internal/store/models.go      — Domain types: User, Game, Move, GameStatus, Color, Outcome, OutcomeReason, GameOutcome
internal/store/postgres.go    — NewPool (pgxpool initialization with ping)
internal/store/game_store.go  — CreateGame, GetGame, UpdateGameStatus, UpdateCurrentFEN, UpdatePlayerBlack, GetActiveGames, UpdateClocks
internal/store/move_store.go  — SaveMove (RETURNING id/played_at), GetMovesForGame (ASC order)
internal/store/user_store.go  — CreateOrGetUser (upsert), GetUser
internal/auth/token.go        — PlayerClaims, SignPlayerToken, VerifyPlayerToken (HS256, algorithm confusion prevention)
internal/api/routes.go        — chi router, middleware stack [Step 12]
internal/api/game_handler.go  — POST /games, POST /games/:id/join, GET /games/:id, GET /health [Step 12]
migrations/                   — SQL migration files (golang-migrate format)
```

---

## Chess Layer Key Patterns

**Validate-then-apply split (ADR-013) with FEN computation between:**
```go
// Step 1: validate — no mutation, before DB write
if err := validator.ValidateMove(session.board, san); err != nil {
    return &MoveRejectionError{Reason: RejectReasonIllegalMove}
}

// Step 2: compute fenAfter — no mutation, uses Position.Update
fenAfter, err := chess.ComputeFENAfterMove(session.board, san)

// Step 3: persist — DB write while board is unchanged
if err := moveStore.SaveMove(ctx, move); err != nil {
    return fmt.Errorf(...) // plain error, not MoveRejectionError
}

// Step 4: apply — mutate only after DB confirms
session.mu.Lock()
validator.ApplyMove(session.board, san)
session.mu.Unlock()

// Step 5 (Step 9 addition): switch clock after board mutation
session.clock.Switch()
```

**MoveRejectionError — always use errors.As at the call site:**
```go
var rejection *game.MoveRejectionError
if errors.As(err, &rejection) {
    // send MOVE_REJECTED with rejection.Reason
} else {
    // infrastructure failure — log, send ERROR to client
}
```

**MoveHistory — always use the package-level wrapper, never `g.MoveHistory()`:**
```go
// CORRECT
sans := chess.MoveHistory(session.board)  // uses internal/chess package-level function

// WRONG — panics on nil comments slice for non-PGN games
sans := session.board.MoveHistory()
```

**DetectOutcome — always call after ApplyMove:**
```go
if err := validator.ApplyMove(session.board, san); err != nil { ... }
if outcome, ended := validator.DetectOutcome(session.board); ended {
    // handle game over
}
```

**Chess layer API split — package-level vs. Validator methods:**
```
Package-level functions (no Validator needed):
  chess.NewGame() *chess.Game
  chess.GameFromFEN(fen) (*chess.Game, error)
  chess.GameFromMoves(moves) (*chess.Game, error)
  chess.CurrentFEN(g) string
  chess.MoveHistory(g) []string
  chess.ComputeFENAfterMove(g, san) (string, error)   ← Added Step 8

Methods on *Validator (used in move pipeline):
  validator.ValidateMove(g, san) error
  validator.ApplyMove(g, san) error
  validator.DetectOutcome(g) (*GameOutcome, bool)
```

---

## GameSession Key Patterns

**State machine — only Transition() changes status:**
```go
// Valid edges only:
WAITING_FOR_PLAYER → ACTIVE
ACTIVE             → COMPLETED
ACTIVE             → ABANDONED
// COMPLETED and ABANDONED are terminal.
```

**Connection lifecycle:**
```go
// First connect:
session.RegisterConnection(color, conn)  // error if slot occupied

// Reconnect:
session.ReplaceConnection(color, conn)   // unconditional overwrite

// Disconnect:
session.ClearConnection(color)           // sets slot to nil
```

**Sending messages — always through session, never via raw *ws.Connection:**
```go
session.SendToPlayer(store.ColorWhite, msgBytes)  // one player
session.SendToBothPlayers(msgBytes)                // broadcast
```

**Accessing session.board in move.go (same package):**
```go
// ValidateMove + ComputeFENAfterMove: single RLock acquisition
session.mu.RLock()
validateErr = validator.ValidateMove(session.board, san)
if validateErr == nil {
    fenAfter, computeErr = chess.ComputeFENAfterMove(session.board, san)
}
session.mu.RUnlock()

// ApplyMove: write lock
session.mu.Lock()
applyErr = validator.ApplyMove(session.board, san)
session.mu.Unlock()

// Clock switch: after ApplyMove, before DetectOutcome
session.clock.Switch()
whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()

// DetectOutcome: no lock — pure read after exclusive write is complete,
// and only one goroutine calls ProcessMove per session at a time
outcome, ended = validator.DetectOutcome(session.board)
```

**Constructing a session — two paths:**
```go
// New game (Manager.CreateGame):
session := NewGameSession(gameID, whiteUserID)
// board = starting position, clock = NewClock(InitialTimeMs), status = WAITING_FOR_PLAYER

// Restored from DB (Manager.RestoreActiveGames → restoreGame):
session := NewGameSessionFromDB(game, board)
// board = replayed via chess.GameFromMoves; clock = NewClockWithTimes(whiteMs, blackMs)
// from persisted values; clock is NOT started — starts on first HandleConnect
// once both players reconnect.
```

**ReadLoop wiring (preview for Step 11):**
```go
// NOTE: Review context lifetime at Step 11 before using this pattern.
// r.Context() is cancelled when ServeHTTP returns — may be too short.
go conn.ReadLoop(
    func(msg []byte) { manager.HandleMessage(ctx, gameID, color, msg) },
    func() {
        manager.HandleDisconnect(gameID, color)
        wsRegistry.Unregister(conn.ID)
    },
)
```

---

## Clock Key Patterns

**Constructors — two entry points:**
```go
clock := game.NewClock(InitialTimeMs)              // new game, both sides equal
clock := game.NewClockWithTimes(whiteMs, blackMs)  // restored from DB, independently diverged
```

**Lifecycle — Start is idempotent, must register callback first:**
```go
clock.SetTimeoutCallback(func(timedOut store.Color) {
    manager.handleTimeout(gameID, timedOut)  // must NOT call any Clock method
})
clock.Start(store.ColorWhite)  // no-op if already started
```

**Switch / Pause / Resume — all no-ops if clock not started; Pause/Resume also no-op on mismatched paused state:**
```go
clock.Switch()           // after ApplyMove succeeds in ProcessMove
clock.Pause()             // on player disconnect (TD-002)
clock.Resume(activeColor) // on reconnect; resumes c.active regardless of argument mismatch
```

**Reading remaining time — always accounts for in-flight elapsed:**
```go
remaining := clock.TimeRemaining(store.ColorWhite)  // accurate even mid-countdown
```

**Stopping — always before a terminal state transition:**
```go
session.clock.Stop()  // in handleGameOver, handleResign, handleTimeout, onAbandonTimeout
session.Transition(store.GameStatusCompleted)
```

**Internal goroutine discipline — `run()` never touches `c.mu`:**
All exported methods (`Start`, `Switch`, `Pause`, `Resume`) compute the new timer duration under `c.mu`, then send a `clockReset{duration, color}` via the buffered(1) `resetCh`. The background `run()` goroutine is the sole owner of the active `*time.Timer` and reacts only to `resetCh`/`stopCh`/`timerC`.

---

## Manager Key Patterns

**Game ID generation — UUID v7, not v4:**
```go
gameUUID, err := uuid.NewV7()  // time-ordered, correct for DB primary key
gameID := gameUUID.String()
```

**Three distinct paths through HandleConnect:**
```
1. First connect (status == WAITING_FOR_PLAYER, slot was empty):
   register → if both connected: Transition(ACTIVE), persist, clock.Start(WHITE)
            → send GAME_STATE to connector, OPPONENT_CONNECTED to opponent

2. Reconnect (status == ACTIVE, slot occupied by stale conn OR explicit reconnect):
   replace connection → cancel abandon timer → send GAME_STATE, OPPONENT_RECONNECTED
   → if both connected: clock.IsStarted() ? clock.Resume(turn) : clock.Start(turn)

3. Post-restart reconnect (status == ACTIVE, clock never started in this process):
   same as path 2, but clock.IsStarted() returns false → clock.Start(turn) is used
   instead of Resume — this is why IsStarted() exists on Clock.
```

**RestoreActiveGames — defensive reconstruction, never trusts current_fen:**
```go
moves, _ := moveStore.GetMovesForGame(ctx, gameID)
board, _ := chess.GameFromMoves(sans)  // NEVER chess.GameFromFEN(game.CurrentFEN)

if outcome, ended := validator.DetectOutcome(board); ended {
    // zombie ACTIVE: correct DB, do NOT add to registry
    gameStore.UpdateGameStatus(ctx, gameID, COMPLETED, outcome)
    return
}
// otherwise: hydrate session, subscribe to EventBus, register
session := NewGameSessionFromDB(game, board)
registry.Register(session)
```

**Game-over paths all follow the same shape — stop clock, transition, persist (non-fatal), publish:**
```go
session.clock.Stop()
session.Transition(store.GameStatusCompleted)  // or ABANDONED
session.SetOutcome(outcome, reason)
gameStore.UpdateGameStatus(ctx, gameID, status, &store.GameOutcome{...})  // log on failure, continue
publishGameOver(ctx, session, outcome, reason, fen)  // via EventBus; falls back to direct send on publish failure
```

**EventBus subscriber — self-terminating on GAME_OVER:**
```go
go func() {
    defer unsubscribe()
    for event := range ch {
        session.SendToBothPlayers(event.Payload)
        if event.Type == MsgTypeGameOver {
            return  // unsubscribe() closes ch via defer; loop would exit anyway
        }
    }
}()
```

---

## EventBus Key Patterns

**Publish (move pipeline / manager game-over paths):**
```go
payload, _ := json.Marshal(moveAppliedMsg{...})
bus.Publish(ctx, game.GameEvent{
    GameID:  session.ID,
    Type:    game.MsgTypeMoveApplied, // or MsgTypeGameOver
    Payload: payload,
})
```

**Subscribe (manager, on game creation or restore):**
```go
ch, unsubscribe, err := bus.Subscribe(ctx, gameID)
m.startEventSubscriber(session, ch, unsubscribe)  // self-terminating goroutine, see above
```

**DO NOT optimise LocalEventBus.Publish to snapshot-then-send:**
The read lock must be held for the entire send loop. See Known Sharp Edges.

---

## Store Layer Key Patterns

**scanGame helper** — works for both QueryRow and Query rows.

**Nullable column scanning** — scan as `*string`, convert to typed pointer.

**Empty slice convention** — always return `make([]*T, 0)`, never `var x []*T`.

---

## Auth Layer Key Patterns

**Algorithm confusion prevention** — keyFunc must reject non-HMAC methods.

**Error sentinel wrapping** — `fmt.Errorf("%w: %v", ErrTokenInvalid, err)`.

---

## Next Recommended Task

**Step 11: WebSocket Handler — implement `internal/ws/handler.go`.**

**Before writing any code**, resolve the context-lifetime decision flagged repeatedly in Known Sharp Edges: `r.Context()` is cancelled when `ServeHTTP` returns, but `ReadLoop` (and the `HandleMessage`/`HandleDisconnect` callbacks it drives) continue running after `ServeHTTP` returns. Options to weigh explicitly before implementation:
1. Pass `context.Background()` to every `HandleMessage` call from within the read loop — simplest, but loses any request-scoped cancellation/tracing.
2. Derive a long-lived context at `Handler` construction time (tied to server lifetime, cancelled on graceful shutdown) and pass that instead — correct for Phase 1's single-process model, sets up the pattern Step 13's graceful shutdown will need.
3. Some hybrid — unlikely to be justified at this phase.

Recommendation to evaluate at session start: Option 2, since Step 13 (Main and Wiring) explicitly requires "wait for in-progress moves to complete" on shutdown — a server-lifetime context that can be cancelled on SIGTERM is the natural backbone for that requirement. This should be discussed and decided before any code is written, not discovered mid-implementation.

**Then implement:**
- `Handler` struct — depends on `*auth.TokenService` (or equivalent verify function/secret), `*game.Manager`, `*ws.Registry`
- `ServeHTTP(w http.ResponseWriter, r *http.Request)`:
  - Extract token from `?token=` query parameter
  - Verify token via `auth.VerifyPlayerToken`; on failure, respond 401 before upgrading (per PHASE_1.md connection flow)
  - Verify `claims.GameID` matches the `:id` URL parameter; mismatch → 401
  - Upgrade to WebSocket via gorilla/websocket upgrader
  - Assign connection ID, register into `ws.Registry`
  - Call `game.Manager.HandleConnect(ctx, gameID, color, conn)`
  - Wire `conn.ReadLoop(onMessage, onClose)`:
    - `onMessage` → `game.Manager.HandleMessage(ctx, gameID, color, raw)`
    - `onClose` → `game.Manager.HandleDisconnect(gameID, color)`, then `ws.Registry.Unregister(connID)`

**Integration tests** (httptest.NewServer per CODING_GUIDELINES.md §6):
- Invalid token → connection refused before upgrade, correct HTTP status / close code
- Valid token → connection accepted, `GAME_STATE` message received
- Second connection with the same token (reconnection) → current `GAME_STATE` delivered, original connection's session state reflected correctly

**Also recommended for this same session or immediately after:** write Manager integration tests (`CreateGame`, `JoinGame`, `RestoreActiveGames`) against real PostgreSQL — these exist as a checklist gap from Step 10 and should not be deferred indefinitely, since Step 11's tests will depend on `Manager.CreateGame`/`JoinGame` working correctly to set up test fixtures.

Estimated 2-3 hours: the context-lifetime decision requires deliberate review (not just an implementation detail), and untested integration surface area (Manager) compounds with new Handler code if not addressed first.

---

## Session Log

| Session | Date | What Was Done |
|---------|------|---------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only |
| 3 | 2025-01-XX | Step 1 scaffold complete |
| 4 | 2025-01-XX | Steps 2–4 complete: migrations, store layer, auth layer |
| 5 | 2025-01-XX | Step 5 complete: chess layer, ADR-013, notnil/chess workarounds discovered |
| 6 | 2026-06-27 | ws port (ReadLoop callback, TextMessage, slog), Steps 6–7 complete: GameSession, GameRegistry, EventBus, messages.go |
| 7 | 2026-06-28 | Step 8 complete: MoveProcessor, MoveRejectionError, ComputeFENAfterMove, 5 integration tests passing with -race |
| 8 | 2026-06-30 | Step 9 complete: Clock (channel-based, goleak-verified), wired into session/move pipeline. Step 10 complete: Manager (CreateGame, JoinGame, HandleConnect/Disconnect/Message, RestoreActiveGames with stale-FEN and zombie-ACTIVE handling, abandonment timers, resign/timeout/abandon paths). Switched game ID generation from hand-rolled UUID v4 to google/uuid v7 mid-session after review. All unit tests passing with -race. |
```