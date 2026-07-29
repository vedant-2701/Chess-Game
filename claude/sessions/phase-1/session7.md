## Session Summary — Fixed abandonment logic, two concurrency races

### What Was Built

- **Abandonment semantics correction:** `GameSession.IsPlayerConnected(color store.Color) bool` added to `session.go` (read-locked check of the relevant connection slot). `Manager.onAbandonTimeout` rewritten to branch on the opponent's connection state at the moment the 60-second timer fires: opponent still connected → `ACTIVE → COMPLETED`, opponent wins, `outcome_reason: ABANDONED`; opponent also disconnected → `ACTIVE → ABANDONED`, `DRAW`, `outcome_reason: ABANDONED`.
- **Registry cleanup centralization:** `Manager.finalizeGame(gameID)` added — cancels both colors' abandon timers and calls `registry.Unregister(gameID)`. Called from exactly three sites: `handleResign`, both branches of the corrected `onAbandonTimeout`, and `HandleMessage`'s `MsgTypeMove` case (gated on a new `gameEnded` signal).
- **`MoveProcessor.ProcessMove` signature changed** from `(err error)` to `(gameEnded bool, err error)` so `HandleMessage` knows when to call `finalizeGame`. Updated the single call site in `manager.go` and all seven call sites in `move_test.go`, including a new assertion on `gameEnded` in the checkmate test.
- **Verified ADR-014** (TOCTOU re-check under `session.mu.Lock()` in `ProcessMove`) was already correctly implemented outside this conversation — reviewed against the code directly, not re-implemented.
- **Fixed a `goleak` false positive** against `pgxpool`'s `backgroundHealthCheck` goroutine in `clock_test.go`, using `goleak.IgnoreTopFunction`, verified against the actual vendored symbol via `gopls:go_search` (`github.com/jackc/pgx/v5@v5.7.1/pgxpool.(*Pool).backgroundHealthCheck`) rather than trusting web search alone. Replaced a rejected package-level-mutable-flag approach (`integrationTestsActive`) that violated `CODING_GUIDELINES.md` §5 and would have silently disabled all Clock leak detection under `-tags integration`.
- **`internal/game/manager_test.go` written and passing:** integration tests for `CreateGame` (persistence, distinct UUIDv7 IDs, token claims), `JoinGame` (persistence, self-play rejection, already-joined rejection, nonexistent-game rejection), and `RestoreActiveGames` (in-progress hydration, stale-`current_fen` rejection in favor of `GameFromMoves`, zombie-ACTIVE detection/correction, WAITING-status restore, genuinely-COMPLETED-games-skipped, per-game failure isolation).
- **Real bug found by your own test run:** `JoinGame`'s only precondition was `status != WAITING_FOR_PLAYER`, insufficient because status stays `WAITING_FOR_PLAYER` until both WebSockets connect — a second user could silently overwrite `player_black_id`. You added `PlayerBlackID != nil` as a guard.
- **Follow-on concurrency bug identified and fixed:** the above guard closed the sequential case only. `UpdatePlayerBlack`'s SQL had no `WHERE` predicate tying the write to the precondition, so two concurrent `JoinGame` calls could both pass the pre-flight check before either committed. Fixed with an atomic conditional `UPDATE ... WHERE id = $2 AND status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL`.
- **Follow-on correctness bug in that fix:** `RowsAffected() == 0` originally returned `store.ErrGameNotFound`, conflating "row missing" with "row exists, precondition failed" — a direct violation of `CODING_GUIDELINES.md` §1. Added `store.ErrGameNotJoinable` to `internal/store/errors.go`, updated `UpdatePlayerBlack` to return it, added `errors.Is` translation in `Manager.JoinGame` to `game.ErrGameNotJoinable` so no caller depends on a store-package sentinel (preserves `game → store` dependency direction).
- **`manager_race_test.go` written and passing under `-race`:** `TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins` — 20 trials, two real goroutines racing `JoinGame` against a fresh game each trial, asserting exactly one success/one `ErrGameNotJoinable`, cross-checked against both the DB row and the in-memory session. Confirmed passing by you, along with the full `internal/game` suite.
- **Documentation corrected:** `PHASE_1.md` and `ARCHITECTURE.md` state-machine sections and `GAME_OVER` protocol notes updated to reflect corrected abandonment semantics. `DECISIONS_LOG_PHASE_1.md` gained **ADR-015** (abandonment correction) and **ADR-016** (JoinGame race fix), both with full options-considered sections.

### Decisions Made

- **ADR-015 (logged):** Single-player disconnect >60s with opponent connected → opponent wins (`COMPLETED`); both disconnected → drawn `ABANDONED`. Rejected keeping the literal original spec (Option A: leaves a game stuck `ACTIVE` forever if one player never returns) and rejected asymmetric timer durations (Option C: unscoped complexity).
- **ADR-016 (logged):** Atomic conditional `UPDATE` as the sole correctness guarantee for `JoinGame`, not a `SELECT ... FOR UPDATE` transaction (Option A — would require ad hoc transaction management leaking into `internal/game`, violating the store-layer SQL boundary) or an advisory lock (Option C — unscoped general-purpose primitive for a single-write problem).
- **No shared "was this checked for concurrent callers" helper introduced** — ADR-016 explicitly notes this is now a standing question for any new read-then-write sequence in `game`/`store`, not a new abstraction or lint rule.

### Tradeoffs Considered

- Kept the pre-flight `GetGame` + `PlayerBlackID != nil` check in `JoinGame` even after the atomic `UPDATE` made it non-load-bearing for correctness — retained purely for `ErrSelfPlay`/`ErrGameNotJoinable` error specificity in the non-racing common case.
- `finalizeGame` cancels both colors' abandon timers unconditionally on every terminal path (including checkmate/resign, where no timer may exist) rather than conditionally — accepted as a safe, always-idempotent no-op rather than adding branching to avoid a cheap redundant call.

### Lessons Learned

- A sequential-only test (`TestManager_JoinGame_RejectsWhenAlreadyJoined`) can pass while a real concurrency bug remains underneath it; proving the fix required a dedicated concurrent-goroutines test against real PostgreSQL, since the race is DB-level and invisible to `go test -race`.
- `gopls` cannot index files under non-default build tags (`//go:build integration`) — `go_diagnostics`, `go_file_context`, and `go_search` all report empty/no-metadata for these files regardless of which gopls tool is used. Verification of integration-tagged files requires the actual `go test -tags integration` run on your machine; this was underestimated earlier in the session and cost a round trip.
- Symbol verification for third-party library internals (e.g., `pgxpool.(*Pool).backgroundHealthCheck`) should go through `gopls:go_search` against your actual vendored source first, not web search — you correctly flagged this after I did it the slower way.
- Editing files containing box-drawing characters (state machine diagrams) via `Filesystem:edit_file`'s exact-string matching is fragile; retyped Unicode box-drawing characters can silently mismatch whitespace. Safer pattern: `view_range` to extract the literal block, then use that exact text as `oldText`, or avoid touching the diagram and append a correction note instead (done for `ARCHITECTURE.md`).

### Problems Encountered

- My own signature change to `ProcessMove` broke `move_test.go` (an existing, already-passing integration test file) — all seven call sites needed updating in the same session before anything would compile. Caught before handing back to you, but should have been done atomically with the signature change, not as an afterthought.
- Initial `manager_test.go` draft assumed test helpers (`mustCreateUser`, `truncateAll`, `testPool`) without having freshly confirmed their existence in `testmain_test.go` in this exact session — turned out correct, but the confirmation came from re-reading the file, not from `gopls`, which reported false negatives on build-tagged files.
- `Filesystem:read_text_file` with a `tail` parameter on `DECISIONS_LOG_PHASE_1.md` initially targeted the wrong path (repo root instead of the actual `claude/claude_web_project/` subfolder) — corrected via `list_directory`.
- No unresolved problems carry into the next session. `move_test.go` is confirmed compiling and passing (per your full-suite run). ADR-016 is now written and no longer a dangling reference.

### Checklist Progress

- ✅ ADR-014 verified (pre-existing, not new this session)
- ✅ ADR-015 written and applied: abandonment semantics correction (`GameSession.IsPlayerConnected`, `onAbandonTimeout` rewrite)
- ✅ `Manager.finalizeGame` — registry cleanup centralized across three call sites
- ✅ `ProcessMove` signature change to `(gameEnded bool, err error)`, all call sites updated
- ✅ `goleak`/`pgxpool` false-positive fixed via `IgnoreTopFunction`, package-level-flag approach rejected and removed
- ✅ `internal/game/manager_test.go` — `CreateGame`, `JoinGame`, `RestoreActiveGames` integration tests, all passing
- ✅ ADR-016 written and applied: `JoinGame` atomic conditional UPDATE, `store.ErrGameNotJoinable` sentinel
- ✅ `internal/game/manager_race_test.go` — concurrent-join regression test, passing under `-race`
- ✅ `PHASE_1.md`, `ARCHITECTURE.md` state-machine sections corrected to match ADR-015
- 🔄 CLAUDE.md full rewrite — this document (Part 2 below)
- ❌ Step 11 (`internal/ws/handler.go`) — not started; ReadLoop context-lifetime decision still undecided

### Technical Debt Introduced

None new this session. All changes were correctness fixes to already-"complete" Step 10 work, not new shortcuts. TD-001 through TD-007 (see CLAUDE.md) remain unchanged and accurate.

### Files Modified

**Created:**
- `internal/game/manager_test.go`
- `internal/game/manager_race_test.go`

**Modified:**
- `internal/game/session.go` — added `IsPlayerConnected`
- `internal/game/manager.go` — `onAbandonTimeout` rewritten, `finalizeGame` added and wired into three call sites, `HandleMessage`'s MOVE branch updated for new `ProcessMove` signature, `JoinGame` gained `store.ErrGameNotJoinable` translation
- `internal/game/move.go` — `ProcessMove` signature changed to `(gameEnded bool, err error)`
- `internal/game/move_test.go` — seven call sites updated for new signature, one new `gameEnded` assertion added
- `internal/game/clock_test.go` — `goleak.IgnoreTopFunction` fix applied (per your report; exact diff not re-verified by me in this session beyond confirming the target symbol)
- `internal/store/game_store.go` — `UpdatePlayerBlack` SQL made an atomic conditional UPDATE, error handling corrected to return `ErrGameNotJoinable` instead of `ErrGameNotFound`
- `internal/store/errors.go` — added `ErrGameNotJoinable`
- `claude/claude_web_project/PHASE_1.md` — Game State Machine section and `GAME_OVER` reason note corrected
- `claude/claude_web_project/ARCHITECTURE.md` — Game State Machine and WebSocket Connection Lifecycle sections corrected
- `claude/claude_web_project/DECISIONS_LOG_PHASE_1.md` — ADR-015 and ADR-016 appended

**Deleted (per your report, not independently verified by me):**
- `internal/game/integration_flag.go`, `internal/game/integration_flag_off.go` — removed in favor of `goleak.IgnoreTopFunction`

### Recommended Next Step

**Step 11: `internal/ws/handler.go`.** Before writing code, explicitly decide the `ReadLoop` context-lifetime question flagged repeatedly in Known Sharp Edges: `r.Context()` is cancelled when `ServeHTTP` returns, but `ReadLoop` and the `HandleMessage`/`HandleDisconnect` callbacks it drives continue running after that. Recommended: a server-lifetime context created at `Handler` construction, cancelled on SIGTERM — this is also the natural backbone Step 13's graceful shutdown will need ("wait for in-progress moves to complete"). Then implement `Handler` struct, `ServeHTTP` (token extraction, verification, `claims.GameID` vs URL match, upgrade, registration, `HandleConnect`, `ReadLoop` wiring), plus httptest-based integration tests per `CODING_GUIDELINES.md` §6: invalid token refused pre-upgrade, valid token receives `GAME_STATE`, reconnection with the same token receives current `GAME_STATE`. Estimated 2-3 hours.

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
**Status: 🔄 In Progress — Steps 1–10 Complete and hardened (two correctness bugs found and fixed via review/testing), Step 11 (WebSocket Handler) Next**

---

## Completed Work

### Documentation
- [x] Project purpose and scope defined
- [x] Full tech stack decided and rationale documented
- [x] All documentation files created
- [x] Phase 1 spec written (PHASE_1.md)
- [x] Architecture documented (ARCHITECTURE.md)
- [x] All ADRs logged (DECISIONS_LOG_PHASE_1.md) — now through **ADR-016**
- [x] `turn` field casing corrected to uppercase (`"WHITE"`/`"BLACK"`) in CLAUDE.md — PHASE_1.md is authoritative
- [x] `GET /games/:id` added to ARCHITECTURE.md endpoint list
- [x] Clock spec corrected to include `startedAt time.Time` field and precise `Resume` semantics before Step 9 implementation began
- [x] Step 10 sharp edges (stale `current_fen`, zombie ACTIVE games) documented before Step 10 implementation began
- [x] **PHASE_1.md and ARCHITECTURE.md Game State Machine sections corrected** to reflect ADR-015 abandonment semantics (single-disconnect vs. both-disconnected produce different terminal states)

### Implementation
- [x] WebSocket infrastructure (`internal/ws`): connection lifecycle, read loop (callback-based), write loop, heartbeats, registry, graceful shutdown — ported from learning project and updated for production use
- [x] Step 1: Project Scaffold — go.mod, docker-compose.yml, .env.example, Makefile, directory structure
- [x] Step 2: Database Migrations — 3 up/down pairs (users, games, moves), verified migrate-up/down/idempotency
- [x] Step 3: Store Layer — postgres.go, game_store.go, move_store.go, user_store.go + integration tests (all passing with -race)
- [x] Step 4: Auth Layer — token.go (SignPlayerToken, VerifyPlayerToken) + unit tests (all passing with -race)
- [x] Step 5: Chess Layer — errors.go, types.go, validator.go + 20 unit tests (all passing with -race)
- [x] Step 6: Game Session and Registry — session.go, registry.go, errors.go + unit tests (all passing with -race)
- [x] Step 7: EventBus — eventbus.go, messages.go + unit tests (all passing with -race)
- [x] Step 8: Move Pipeline — move.go (MoveProcessor, MoveRejectionError, ComputeFENAfterMove) + integration tests (all passing with -race)
- [x] Step 9: Clock — clock.go (channel-based, goroutine-per-clock), wired into session.go and move.go, clock_test.go unit tests passing with -race and goleak (pgxpool false-positive resolved via `goleak.IgnoreTopFunction`, see Known Sharp Edges)
- [x] Step 10: Manager — manager.go (CreateGame, JoinGame, HandleConnect, HandleDisconnect, HandleMessage, RestoreActiveGames, abandonment timers, resign/timeout/abandon game-over paths, `finalizeGame` registry cleanup)
- [x] **Pre-Step-11 hardening pass:** ADR-014 (TOCTOU re-check in ProcessMove) verified correct; ADR-015 (abandonment semantics correction) designed, implemented, tested; ADR-016 (JoinGame concurrent-join race) found, fixed, tested; Manager integration test suite written from scratch (was a complete gap through Step 10)

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
- [x] GameState machine (WAITING → ACTIVE → COMPLETED / ABANDONED) — **corrected transition semantics per ADR-015**
- [x] GameRegistry (gameID → *GameSession)
- [x] EventBus interface defined (LocalEventBus for Phase 1)
- [x] Player-to-connection bridge for reconnection (session pointer slots + ReplaceConnection)
- [x] `GameSession.IsPlayerConnected(color)` — added for ADR-015, used by onAbandonTimeout
- [x] messages.go — single source of truth for all WebSocket protocol strings
- [x] MoveProcessor — full 8-step pipeline (Step 8), clock-switching wired in (Step 9), TOCTOU re-check under session.mu.Lock() before ApplyMove (ADR-014), **returns `(gameEnded bool, err error)`**
- [x] MoveRejectionError — typed client-facing rejection error (Step 8)
- [x] Clock — per-game countdown timers, timeout detection, channel-based goroutine (Step 9)
- [x] Manager — top-level orchestrator: game creation/join, connection lifecycle, message routing, restart recovery (Step 10), **`finalizeGame` centralized registry cleanup**, **`onAbandonTimeout` corrected for single- vs. both-disconnected**

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
- [x] Re-check session status under lock immediately before ApplyMove (ADR-014 — closes TOCTOU with concurrent resign/timeout/abandon)

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

### Abandonment (ADR-015 correction)
- [x] Single-player disconnect >60s with opponent connected → opponent wins (`COMPLETED`, `outcome_reason: ABANDONED`)
- [x] Both-players-disconnected >60s → drawn abandonment (`ABANDONED`, `outcome: DRAW`, `outcome_reason: ABANDONED`)
- [x] `GameSession.IsPlayerConnected` used to distinguish the two cases at timer-fire time
- [x] `finalizeGame` called on both branches — no dangling abandon timers or un-unregistered sessions

### Persistence Recovery
- [x] On server restart, active games are recoverable from DB — RestoreActiveGames
- [x] GameSession can be hydrated from DB records — NewGameSessionFromDB
- [x] In-progress games resume correctly after server restart — board via GameFromMoves, zombie ACTIVE games corrected
- [x] **Covered by integration tests** (manager_test.go): stale-FEN handling, zombie-ACTIVE correction, WAITING-status restore, per-game failure isolation, genuinely-COMPLETED games correctly excluded

### Testing
- [x] Store layer: integration tests with real PostgreSQL (integration build tag)
- [x] Auth layer: unit tests (no build tag, no database)
- [x] Chess layer: unit tests (no build tag, no database)
- [x] Game session and registry: unit tests (no build tag, no database)
- [x] EventBus: unit tests (no build tag, no database)
- [x] Move pipeline: integration tests (integration build tag, real PostgreSQL), including gameEnded-signal assertion
- [x] Clock: unit tests with goleak goroutine-leak verification, pgxpool false-positive resolved via `goleak.IgnoreTopFunction` (no build tag for the tests themselves, but must pass correctly when run alongside `-tags integration`)
- [x] **Manager: integration tests (CreateGame, JoinGame, RestoreActiveGames) — `manager_test.go`, written this session, all passing**
- [x] **Manager: concurrency regression test — `manager_race_test.go`, `TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins`, 20 trials, passing under `-race`**
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
| ADR-014 | ProcessMove TOCTOU fix | Re-check session status under `session.mu.Lock()` immediately before ApplyMove |
| ADR-015 | Abandonment semantics correction | Single-disconnect (opponent connected) → COMPLETED, opponent wins. Both-disconnected → ABANDONED, DRAW. |
| ADR-016 | JoinGame double-join race | Atomic conditional `UPDATE ... WHERE status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL`, disambiguated via new `store.ErrGameNotJoinable` sentinel |

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
| `finalizeGame(gameID)` centralization | One method, three call sites (`handleResign`, both `onAbandonTimeout` branches, `HandleMessage`'s MOVE case via `gameEnded`) | Prevents registry-cleanup omission recurring a fourth time (e.g. a future draw-agreement path); `cancelAbandonTimer` and `registry.Unregister` are both safe no-ops on missing keys, so unconditional calls on every terminal path are cheap and correct |
| `ProcessMove` returns `(gameEnded bool, err error)` | Not error-only | `HandleMessage` needs to know when to call `finalizeGame`; `gameEnded` is true whenever `handleGameOver` ran, even if its internal DB persist failed (in-memory transition + GAME_OVER publish already happened) |
| `GameSession.IsPlayerConnected(color)` | Read-locked check of one connection slot | Used exclusively by `onAbandonTimeout` (ADR-015) to determine the opponent's connection state at the moment the abandon timer fires |
| `goleak.IgnoreTopFunction` over a package-level `integrationTestsActive` flag | Targeted exemption of `pgxpool.(*Pool).backgroundHealthCheck` by exact symbol name (verified via `gopls:go_search` against vendored `pgx/v5@v5.7.1`) | A package-level mutable bypass flag would violate `CODING_GUIDELINES.md` §5 and silently disable all Clock goroutine-leak detection whenever `-tags integration` is present — exactly the acceptance-criterion-#7 check this project worked hardest to establish |
| `store.ErrGameNotJoinable` as a new sentinel, not reuse of `game.ErrGameNotJoinable` | Added to `internal/store/errors.go` | `internal/store` must not import `internal/game` (dependency graph is `game → store` only); `Manager.JoinGame` translates via `errors.Is` at the package boundary |

---

## Technical Debt

```
TD-001: Player token passed in URL query parameter (visible in logs) | Phase 1 | Fix by: Phase 3
TD-002: Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 | Fix by: Phase 4
        Corollary (Step 10): boundary race between ProcessMove-driven completion
        and Clock timeout firing at the same instant. Resolved for the specific
        ProcessMove-vs-terminal-transition case by ADR-014's re-check under lock.
        Broader TD-002 root cause (no move timestamps) remains open.
TD-003: No draw offer mechanism | Phase 1 | Fix by: Phase 4
TD-004: Anonymous identity only (no real user accounts) | Phase 1 | Fix by: Phase 3
TD-005: Single time control (10+0 only) | Phase 1 | Fix by: Phase 4
TD-006: DetectOutcome maps ThreefoldRepetition/FiftyMoveRule/InsufficientMaterial to "DRAW_AGREEMENT" | Phase 1 | Fix by: Phase 4
TD-007: GameFromFEN loses position history — threefold repetition blind after server restart | Phase 1 | Fix by: Phase 4
        Note: RestoreActiveGames (Step 10) uses GameFromMoves, not GameFromFEN, for
        exactly this reason — the restored board has full position history. Covered
        by integration tests as of this session (manager_test.go).
```

No new technical debt introduced this session — all changes were correctness fixes to previously "complete" Step 10 work.

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
10. **Any read-then-write sequence spanning two or more statements must be checked for concurrent-caller correctness, not just sequential correctness, before being considered complete.** (ADR-014, ADR-016) A test that only exercises sequential calls does not prove a fix; a dedicated concurrent-goroutines test against real infrastructure is required when the sequence guards a uniqueness or exclusivity invariant.

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://` (for the migrate CLI in the Makefile). When migrations are wired into `main.go` at Step 13, use `pgx5://` scheme with the `golang-migrate/migrate/v4/database/pgx/v5` driver. Different URL schemes, same database — correct and intentional, but will cause confusion at Step 13 if not remembered.

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Any game constructed via `NewGame()` + `MoveStr()` has a nil `comments` field. Do not call `g.MoveHistory()` directly anywhere. Always use the `chess.MoveHistory(g)` wrapper in `internal/chess/validator.go`.

- **`MoveHistory()` returns annotated SAN.** `AlgebraicNotation.Encode` appends `+` for check and `#` for checkmate. The `"moves"` field in `GAME_STATE` messages will contain annotated SAN. Step 8 move pipeline sources move history from `chess.MoveHistory(session.board)` via `CurrentStateSnapshot()`, not from a cached copy of raw client input.

- **`DetectOutcome` must be called after `ApplyMove`.** Calling it before any move is played will always return `(nil, false)` even for checkmate/stalemate positions loaded from FEN.

- **`ReadLoop` context decision deferred to Step 11.** The HTTP request context (`r.Context()`) is cancelled when `ServeHTTP` returns, which is before `ReadLoop` exits. At Step 11, decide whether to pass `r.Context()`, a derived context, or `context.Background()` to `HandleMessage`. The comment in `ReadLoop` flags this explicitly. **This decision must be made deliberately before Step 11 implementation begins, not discovered mid-implementation.** Recommendation carried into this session: a server-lifetime context created at `Handler` construction, cancelled on SIGTERM — aligns with Step 13's graceful shutdown requirement.

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** This is intentional. Do not "optimise" it to snapshot-then-release — that pattern creates a window where `unsubscribe` can close a channel between the snapshot and the send, causing a send-on-closed-channel panic.

- **`ComputeFENAfterMove` must only be called after `ValidateMove` returned nil for the same `(g, san)` pair.** Both use `AlgebraicNotation{}.Decode` internally. If `ValidateMove` passed, `ComputeFENAfterMove` will not error. If it does error, it is a bug — log at Error level and return a plain (non-rejection) error.

- **`move.go` accesses `session.board` and `session.mu` directly.** This is valid because `move.go` is in `package game` — same package as `session.go`. Private fields are accessible within a package. `mu.RLock()` is held for `ValidateMove` + `ComputeFENAfterMove`; `mu.Lock()` is held for the status re-check (ADR-014) and `ApplyMove` together, as a single critical section.

- **`ProcessMove` re-checks `session.status == ACTIVE` under `session.mu.Lock()` immediately before `ApplyMove` (ADR-014).** This closes the TOCTOU window between the pipeline's initial status snapshot (taken before SaveMove) and the point where the board is actually mutated — a concurrent RESIGN, timeout, or abandonment firing in that window is now correctly observed. If the re-check fails, `ApplyMove` is skipped and `ProcessMove` returns `(false, nil)` — the move row is already saved to the `moves` table by this point, which is accepted as a harmless orphaned row (a COMPLETED/ABANDONED game is never replayed by RestoreActiveGames).

- **`onAbandonTimeout` branches on `session.IsPlayerConnected(opponentOf(color))` (ADR-015).** Opponent connected at timer-fire time → `COMPLETED`, opponent wins, `outcome_reason: ABANDONED`. Opponent also disconnected → `ABANDONED`, `DRAW`, `outcome_reason: ABANDONED`. The `status` field (`COMPLETED` vs `ABANDONED`), not `outcome_reason`, is what distinguishes a decisive abandonment-loss from a drawn mutual abandonment — both share the same `outcome_reason` value.

- **`finalizeGame(gameID)` must be called exactly once per game-ending event, after the session has already transitioned to a terminal state and GAME_OVER has already been published.** It is safe to call more than once (both `cancelAbandonTimer` and `registry.Unregister` are no-ops on missing keys) but that safety should not be relied on as a substitute for calling it from the correct single call site per code path. The three current call sites are `handleResign`, both branches of `onAbandonTimeout`, and `HandleMessage`'s MOVE case (gated on `ProcessMove`'s `gameEnded` return value).

- **`JoinGame`'s actual correctness guarantee lives in `GameStore.UpdatePlayerBlack`'s SQL, not in the Go-level pre-flight check (ADR-016).** The `WHERE id = $2 AND status = 'WAITING_FOR_PLAYER' AND player_black_id IS NULL` clause is what makes concurrent joins safe. The pre-flight `GetGame` + `PlayerBlackID != nil` check in `JoinGame` is retained only for producing specific, friendly errors (`ErrSelfPlay` vs `ErrGameNotJoinable`) in the non-racing case — removing it would not reintroduce the race, but would lose error specificity. Do not add new multi-statement read-then-write sequences to `GameStore` without the same atomic-write-as-source-of-truth discipline.

- **`store.ErrGameNotJoinable` is distinct from `store.ErrGameNotFound`.** `UpdatePlayerBlack` returns `ErrGameNotJoinable` when the row exists but its `WHERE` predicate fails (already joined, or not `WAITING_FOR_PLAYER`) — never `ErrGameNotFound` for that condition, since row existence is already established by the caller's prior `GetGame`. `Manager.JoinGame` translates `store.ErrGameNotJoinable` to `game.ErrGameNotJoinable` via `errors.Is` — no caller outside `internal/store` should depend on the store-package sentinel directly.

- **`goleak.IgnoreTopFunction` in `clock_test.go` exempts exactly one symbol: `github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck`.** This goroutine is spawned once by `TestMain`'s pool for the lifetime of the test binary when running with `-tags integration`, and is not a leak from code under test. Verified against the actual vendored `pgx/v5@v5.7.1` source via `gopls:go_search`, not assumed from documentation. If the pgx dependency version changes, re-verify this exact symbol string before trusting the exemption still matches — `IgnoreTopFunction` fails silently (matches nothing, exemption becomes a no-op) rather than erroring on a stale string.

- **`Clock` struct requires a `startedAt time.Time` field — omitted from original Step 9 spec.** `Switch()` and `Pause()` deduct elapsed time since the clock last started from the active player's remaining duration, using `time.Since(startedAt)`. `startedAt` is set on every `Start`/`Resume` call. This is implemented correctly in the current `clock.go` — flagging here for any future reimplementation reference.

- **`Resume(color Color)` restarts the timer for `c.active`, not `color`.** `Pause()` does not clear `c.active`, so it always holds the paused player's color. If `color != c.active` on `Resume`, an Error is logged (caller bug) but `c.active` is NOT overridden. Implemented and tested (`TestClock_Resume_WrongColor_LogsErrorAndResumesActive`).

- **`Clock.run()` never acquires `c.mu`.** All exported methods compute new state under `c.mu`, then send a `clockReset` message via the buffered(1) `resetCh` channel to the background goroutine, which is the sole owner of the `time.Timer`. This avoids the goroutine ever blocking while holding `c.mu`, and avoids `c.mu` being held during a channel send that could theoretically block.

- **`RestoreActiveGames` must reconstruct boards via `chess.GameFromMoves`, never `chess.GameFromFEN(game.CurrentFEN)`.** `UpdateCurrentFEN`'s non-fatal failure mode (Step 8) means `games.current_fen` can be stale relative to the `moves` table, which is the source of truth. Implemented correctly in `manager.go`'s `restoreGame`; covered by `TestManager_RestoreActiveGames_IgnoresStaleCurrentFEN`.

- **`RestoreActiveGames` must detect and correct zombie ACTIVE games.** `handleGameOver`'s decision to publish GAME_OVER even when `UpdateGameStatus` fails (Step 8) can leave a game COMPLETED in memory (and already broadcast as such to players) but still recorded as ACTIVE in the DB. `GetActiveGames` will return it on the next restart. `restoreGame` calls `DetectOutcome` on the replayed board; if the game is already over, it corrects the DB status to COMPLETED and does NOT add the session to the registry. Implemented in `manager.go`; covered by `TestManager_RestoreActiveGames_CorrectsZombieActiveGame`.

- **`gopls` does not index files under non-default build tags (e.g. `//go:build integration`).** `go_diagnostics`, `go_file_context`, and `go_search` all report empty results or "no package metadata" for these files, regardless of which gopls tool is invoked, even when the file compiles correctly. This is a tooling limitation, not a signal of a problem. Verification of integration-tagged files requires an actual `go build -tags integration ./...` or `go test -tags integration ./...` run — gopls cannot substitute for this.

- **Integration tests for `internal/game` use `//go:build integration`.** Running `go test -race ./internal/game/...` runs only unit tests. Running `go test -race -tags integration ./internal/game/...` runs both unit and integration tests (`testmain_test.go`'s `TestMain` sets up `testPool`; unit tests ignore it). Manager now has full integration test coverage (`manager_test.go`, `manager_race_test.go`) as of this session.

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

**Note on clock values in GAME_STATE and MOVE_APPLIED:** As of Step 9/10, all clock values sent to clients are live reads from `session.clock.TimeRemaining()`, accounting for in-flight elapsed time.

**Note on `ABANDONED` outcome pairing (ADR-015):** `reason: "ABANDONED"` can pair with EITHER a winner (`"WHITE"`/`"BLACK"`) or `"DRAW"`, depending on whether one or both players were disconnected when the 60-second window elapsed. The `status` field on the game record (`COMPLETED` vs `ABANDONED`) is what actually distinguishes a decisive abandonment-loss from a drawn mutual abandonment — clients must not infer this from `outcome_reason` alone.

---

## Key Files and Their Responsibilities

```
cmd/server/main.go            — Wires all dependencies, starts HTTP server, handles OS signals
internal/ws/connection.go     — Connection struct, read loop (callback-based), write loop, heartbeat
internal/ws/registry.go       — connID -> *Connection map with RWMutex
internal/ws/errors.go         — ErrConnectionClosed, ErrQueueFull
internal/ws/handler.go        — HTTP upgrade, token validation, GameManager handoff [Step 11 — NEXT]
internal/game/errors.go       — ErrGameNotFound, ErrConnectionOccupied, ErrInvalidTransition,
                                MoveRejectionError, ErrGameNotJoinable, ErrSelfPlay
internal/game/messages.go     — All WebSocket message type strings, rejection reasons, error codes
internal/game/session.go      — GameSession struct, state machine, connection management, snapshots,
                                clock field, NewGameSessionFromDB (restore-from-DB constructor),
                                IsPlayerConnected (ADR-015)
internal/game/registry.go     — gameID -> *GameSession map with RWMutex
internal/game/eventbus.go     — EventBus interface + LocalEventBus implementation
internal/game/manager.go      — Top-level orchestrator: CreateGame, JoinGame, HandleConnect,
                                HandleDisconnect, HandleMessage, RestoreActiveGames,
                                handleResign/handleTimeout/onAbandonTimeout (ADR-015 corrected),
                                finalizeGame (registry cleanup, centralized), abandonment timers
internal/game/manager_test.go       — Integration tests: CreateGame, JoinGame, RestoreActiveGames [NEW]
internal/game/manager_race_test.go  — Concurrency regression: JoinGame race, ADR-016 [NEW]
internal/game/move.go         — MoveProcessor: 8-step pipeline + clock switching (Step 9 wiring),
                                MoveRejectionError, handleGameOver (stops clock), TOCTOU re-check
                                under lock before ApplyMove (ADR-014), returns (gameEnded bool, err error)
internal/game/move_test.go    — Integration tests, updated for ProcessMove's new signature
internal/game/clock.go        — Clock: channel-based per-game countdown timers, timeout detection
internal/game/clock_test.go   — Unit tests with goleak, IgnoreTopFunction exemption for pgxpool health-check goroutine
internal/chess/errors.go      — ErrIllegalMove, ErrInvalidFEN sentinels
internal/chess/types.go       — GameOutcome{Winner, Reason} — chess-layer result type
internal/chess/validator.go   — Validator, NewGame, GameFromFEN, GameFromMoves,
                                ValidateMove, ApplyMove, ComputeFENAfterMove,
                                DetectOutcome, CurrentFEN, MoveHistory
internal/store/errors.go      — ErrGameNotFound, ErrUserNotFound, ErrGameNotJoinable (ADR-016) sentinels
internal/store/models.go      — Domain types: User, Game, Move, GameStatus, Color, Outcome, OutcomeReason, GameOutcome
internal/store/postgres.go    — NewPool (pgxpool initialization with ping)
internal/store/game_store.go  — CreateGame, GetGame, UpdateGameStatus, UpdateCurrentFEN,
                                UpdatePlayerBlack (atomic conditional UPDATE, ADR-016), GetActiveGames, UpdateClocks
internal/store/move_store.go  — SaveMove (RETURNING id/played_at), GetMovesForGame (ASC order)
internal/store/user_store.go  — CreateOrGetUser (upsert), GetUser
internal/auth/token.go        — PlayerClaims, SignPlayerToken, VerifyPlayerToken (HS256, algorithm confusion prevention)
internal/api/routes.go        — chi router, middleware stack [Step 12]
internal/api/game_handler.go  — POST /games, POST /games/:id/join, GET /games/:id, GET /health [Step 12]
migrations/                   — SQL migration files (golang-migrate format)
```

---

## Chess Layer Key Patterns

**Validate-then-apply split (ADR-013) with FEN computation between, plus ADR-014's re-check:**
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

// Step 4 (ADR-014): re-check status under lock, then apply — mutate only after
// DB confirms AND the game is still confirmed ACTIVE at this exact instant
session.mu.Lock()
if session.status != store.GameStatusActive {
    session.mu.Unlock()
    return false, nil // game ended between initial check and this point — skip apply
}
applyErr := validator.ApplyMove(session.board, san)
session.mu.Unlock()

// Step 5 (Step 9 addition): switch clock after board mutation
session.clock.Switch()

// Step 6: return gameEnded so HandleMessage knows whether to call finalizeGame
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
ACTIVE             → COMPLETED   // checkmate, stalemate, timeout, resignation,
                                  // OR single-player abandonment (opponent wins) — ADR-015
ACTIVE             → ABANDONED   // both-players-disconnected only — ADR-015
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

// Abandonment check (ADR-015):
session.IsPlayerConnected(color)         // read-locked slot check, used by onAbandonTimeout
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

// Status re-check (ADR-014) + ApplyMove: single write-lock critical section
session.mu.Lock()
if session.status != store.GameStatusActive {
    session.mu.Unlock()
    return false, nil
}
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
session.clock.Stop()  // in handleGameOver, handleResign, handleTimeout, onAbandonTimeout (both branches)
session.Transition(store.GameStatusCompleted)  // or ABANDONED per ADR-015 branching
```

**Internal goroutine discipline — `run()` never touches `c.mu`:**
All exported methods (`Start`, `Switch`, `Pause`, `Resume`) compute the new timer duration under `c.mu`, then send a `clockReset{duration, color}` via the buffered(1) `resetCh`. The background `run()` goroutine is the sole owner of the active `*time.Timer` and reacts only to `resetCh`/`stopCh`/`timerC`.

**Test leak verification — goleak with a targeted exemption (this session):**
```go
goleak.VerifyNone(t,
    goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
)
```
Applies whenever `clock_test.go` runs in a binary that also links `testmain_test.go`'s `TestMain` (i.e., under `-tags integration`). Do not replace this with a package-level bypass flag — see Non-Negotiable Constraints and CODING_GUIDELINES.md §5.

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

**onAbandonTimeout — corrected branching (ADR-015):**
```go
func (m *Manager) onAbandonTimeout(gameID string, color store.Color) {
    // ... cancel own timer entry, fetch session, snapshot status check ...
    session.clock.Stop()
    opponent := opponentOf(color)
    if session.IsPlayerConnected(opponent) {
        // Opponent present and waiting — disconnected player loses.
        session.Transition(store.GameStatusCompleted)
        session.SetOutcome(store.Outcome(opponent), store.OutcomeReasonAbandoned)
        // persist, publish GAME_OVER, finalizeGame(gameID)
        return
    }
    // Both disconnected — true mutual abandonment, drawn.
    session.Transition(store.GameStatusAbandoned)
    session.SetOutcome(store.OutcomeDraw, store.OutcomeReasonAbandoned)
    // persist, publish GAME_OVER, finalizeGame(gameID)
}
```

**finalizeGame — centralized registry cleanup, three call sites:**
```go
func (m *Manager) finalizeGame(gameID string) {
    m.cancelAbandonTimer(gameID, store.ColorWhite)
    m.cancelAbandonTimer(gameID, store.ColorBlack)
    m.registry.Unregister(gameID)
}

// Call sites:
// 1. handleResign — after publishGameOver
// 2. onAbandonTimeout — after publishGameOver, both branches
// 3. HandleMessage's MsgTypeMove case — after ProcessMove, gated on gameEnded:
gameEnded, moveErr := m.processor.ProcessMove(ctx, session, color, msg.SAN)
// ... handle moveErr (rejection vs infrastructure error) ...
if gameEnded {
    m.finalizeGame(gameID)
}
```

**JoinGame — atomic conditional write is the correctness guarantee (ADR-016):**
```go
game, err := m.gameStore.GetGame(ctx, gameID)          // existence + friendly-error checks
if game.PlayerWhiteID == userID { return "", ErrSelfPlay }
if game.PlayerBlackID != nil { return "", ErrGameNotJoinable }  // fast-path, not authoritative

if err := m.gameStore.UpdatePlayerBlack(ctx, gameID, userID); err != nil {
    if errors.Is(err, store.ErrGameNotJoinable) {
        // Lost a concurrent join race — the atomic UPDATE's WHERE clause
        // is what actually decided this, not the check above.
        return "", fmt.Errorf(..., ErrGameNotJoinable)
    }
    return "", fmt.Errorf(..., err)
}
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

**Game-over paths all follow the same shape — stop clock, transition, persist (non-fatal), publish, finalize:**
```go
session.clock.Stop()
session.Transition(store.GameStatusCompleted)  // or ABANDONED per ADR-015
session.SetOutcome(outcome, reason)
gameStore.UpdateGameStatus(ctx, gameID, status, &store.GameOutcome{...})  // log on failure, continue
publishGameOver(ctx, session, outcome, reason, fen)  // via EventBus; falls back to direct send on publish failure
m.finalizeGame(gameID)  // registry cleanup — every terminal path ends here
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

**Atomic conditional writes for uniqueness/exclusivity invariants (ADR-016).** Any store method that enforces "only one caller may succeed" (e.g. `UpdatePlayerBlack`) must encode the full precondition in the SQL `WHERE` clause and treat `RowsAffected() == 0` as the authoritative failure signal — never rely on a caller's prior `SELECT` to establish correctness under concurrency. Disambiguate "row not found" from "row exists, precondition failed" with distinct sentinels; do not conflate them.

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
- **New, given this session's findings:** consider whether Step 11's tests should also probe the abandonment-timer interaction (a real WebSocket disconnect triggering `onAbandonTimeout`'s corrected branching) now that real connections exist to disconnect — this was untestable without real `*ws.Connection` objects until Step 11 provides them.

**Manager integration tests are now complete** (`manager_test.go`, `manager_race_test.go`) — no longer a blocking gap for Step 11's fixture setup.

Estimated 2-3 hours: the context-lifetime decision requires deliberate review (not just an implementation detail), and this session closed out the previously-blocking Manager test gap, so Step 11 can now proceed without that dependency.

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only |
| 3 | 2025-01-XX | Step 1 scaffold complete |
| 4 | 2025-01-XX | Steps 2–4 complete: migrations, store layer, auth layer |
| 5 | 2025-01-XX | Step 5 complete: chess layer, ADR-013, notnil/chess workarounds discovered |
| 6 | 2026-06-27 | ws port (ReadLoop callback, TextMessage, slog), Steps 6–7 complete: GameSession, GameRegistry, EventBus, messages.go |
| 7 | 2026-06-28 | Step 8 complete: MoveProcessor, MoveRejectionError, ComputeFENAfterMove, 5 integration tests passing with -race |
| 8 | 2026-06-30 | Step 9 complete: Clock (channel-based, goleak-verified), wired into session/move pipeline. Step 10 complete: Manager (CreateGame, JoinGame, HandleConnect/Disconnect/Message, RestoreActiveGames with stale-FEN and zombie-ACTIVE handling, abandonment timers, resign/timeout/abandon paths). Switched game ID generation from hand-rolled UUID v4 to google/uuid v7 mid-session after review. All unit tests passing with -race. |
| 9 | 2026-07-01 | Pre-Step-11 hardening session. Verified ADR-014 (pre-existing). Designed, implemented, and tested ADR-015 (abandonment semantics correction: single- vs. both-disconnected produce different terminal states; `IsPlayerConnected` added; `onAbandonTimeout` rewritten). Centralized registry cleanup into `finalizeGame`, required changing `ProcessMove`'s signature to `(gameEnded bool, err error)`. Fixed a `goleak`/`pgxpool` false positive via `IgnoreTopFunction`, rejecting a package-level-flag approach that would have violated CODING_GUIDELINES.md §5. Wrote `manager_test.go` from scratch (previously a complete gap). Your own test run caught a real double-join bug in `JoinGame`; you fixed the sequential case, I identified and fixed the underlying concurrent-caller race (ADR-016: atomic conditional UPDATE in `UpdatePlayerBlack`, new `store.ErrGameNotJoinable` sentinel) and a follow-on not-found-vs-error conflation bug in that fix. Wrote `manager_race_test.go`, confirmed passing under `-race` with 20 trials. Corrected `PHASE_1.md` and `ARCHITECTURE.md` state-machine sections to match ADR-015. Logged ADR-015 and ADR-016 in full. |
```