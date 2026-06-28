## Session Summary — Move Pipeline and Typed Rejection Errors

### What Was Built

**`internal/chess/validator.go`** — added package-level function `ComputeFENAfterMove(g *chess.Game, san string) (string, error)`. Uses `chess.AlgebraicNotation{}.Decode` to get `*chess.Move`, then `g.Position().Update(move).String()` to obtain the resulting FEN without touching `g`. No game mutation. Consistent with the established package-level function pattern (`CurrentFEN`, `MoveHistory`).

**`internal/game/errors.go`** — added `MoveRejectionError{Reason string}` with `Error() string` method. Used by the Manager (Step 10) to distinguish client-facing rejections from infrastructure failures via `errors.As`.

**`internal/game/move.go`** — full `MoveProcessor` implementation:
- `MoveProcessor` struct depending on `*internalchess.Validator`, `*store.GameStore`, `*store.MoveStore`, `EventBus`
- `NewMoveProcessor(...)` constructor
- `ProcessMove(ctx, session, color, san) error` — 8-step pipeline in exact ADR-013 order
- `handleGameOver(ctx, session, fenAfter, outcome)` — transitions session, persists to DB, publishes `GAME_OVER`
- `publishMoveApplied(ctx, session, san, fenAfter, moveNumber, color, snap)` — marshals and publishes `MOVE_APPLIED`
- Private JSON structs: `moveAppliedMsg`, `gameOverMsg`

**`internal/game/testmain_integration_test.go`** — `TestMain` with DB pool setup, `truncateAll`, `mustCreateUser`, `mustCreateActiveGame` helpers. Integration build tag only.

**`internal/game/move_test.go`** — 5 integration tests, all passing with `-race`:
- `TestMoveProcessor_ValidMove_FullPipeline` — verifies MOVE_APPLIED event, DB move record, DB current_fen, in-memory board and turn advance
- `TestMoveProcessor_WrongTurn_RejectsWithMoveRejectionError` — verifies `*MoveRejectionError` with `RejectReasonNotYourTurn`, board unchanged, nothing persisted
- `TestMoveProcessor_IllegalMove_RejectsAndLeavesBoard` — verifies `*MoveRejectionError` with `RejectReasonIllegalMove`, board unchanged
- `TestMoveProcessor_DBFailure_LeavesBoard` — cancelled context causes `SaveMove` to fail; verifies plain error (not `*MoveRejectionError`), board unchanged (persistence-first invariant)
- `TestMoveProcessor_CheckmateDetected_PublishesGameOver` — Scholar's mate sequence; verifies `GAME_OVER` event with `outcome=WHITE`, `reason=CHECKMATE`, session status `COMPLETED`, DB outcome/reason persisted

### Decisions Made

**`ComputeFENAfterMove` uses `Position.Update(*chess.Move).String()` — no game cloning.** Initial plan was to clone the game via FEN to compute `fenAfter`. During implementation, confirmed that `chess.Position.Update(*chess.Move)` returns `*chess.Position` and `Position.String()` returns FEN. This gives the resulting position without any game allocation or mutation. Cleaner and faster than FEN cloning. No ADR needed — this is an implementation detail of the chess layer, not an architectural decision.

**Single `mu.RLock()` covers both `ValidateMove` and `ComputeFENAfterMove`.** Both operations read `session.board` and use the same `AlgebraicNotation.Decode` path. Acquiring one RLock for both ensures the position cannot change between them and avoids two lock round-trips. `mu.Lock()` is used only for `ApplyMove`.

**`UpdateCurrentFEN` failure is non-fatal.** If `SaveMove` succeeds but `UpdateCurrentFEN` fails, `current_fen` is stale. The move is in the `moves` table (source of truth). Treating this as fatal would leave the board in an inconsistent state (move persisted but not applied in memory). Logged as Error, pipeline continues.

**`handleGameOver` publishes `GAME_OVER` even if DB status update fails.** The in-memory transition has already happened. Players must be notified. DB inconsistency is an error state that must be surfaced in logs, but cannot block player notification.

**Clock values in `MOVE_APPLIED` come from the pre-move snapshot.** Clock switching deferred to Step 9 as agreed. `MOVE_APPLIED.whiteTimeMs` and `MOVE_APPLIED.blackTimeMs` reflect the pre-move clock state until Step 9 wires in the `Clock` object. Not flagged as technical debt — it is the correct sequencing, not a shortcut.

### Tradeoffs Considered

**`ComputeFENAfterMove` via FEN clone vs. `Position.Update`.** FEN clone would create a temporary `*chess.Game` via `GameFromFEN`, apply the move, extract the FEN, and discard it. This allocates a game object per move. `Position.Update` is O(1), allocates only a `*Position`, and avoids any interaction with the game's move history. `Position.Update` is strictly better — chosen.

**`mu.RLock` for read accesses vs. no lock.** The race detector only fires on concurrent read+write, not concurrent read+read. `ValidateMove` and `ComputeFENAfterMove` are reads; holding no lock would pass the race detector since the only concurrent write is `ApplyMove` (protected by `mu.Lock`). However, `session.board` is documented as protected by `mu`. Violating the documented invariant creates a subtle inconsistency even if the race detector doesn't catch it. Holding `mu.RLock` for reads is the correct, documented pattern — chosen.

**`MoveRejectionError` for DB failures vs. separate error type.** PHASE_1.md's pipeline diagram says "DB error → MOVE_REJECTED" for `SaveMove` failure. This would mean using `*MoveRejectionError` even for infrastructure failures. This was rejected: a DB error is not a client-attributable rejection, and surfacing it as "illegal move" or "not your turn" is misleading. Plain error for infrastructure failures, `*MoveRejectionError` for client errors — the Manager decides what to send in each case. This is strictly cleaner.

### Lessons Learned

**`move.go` is in `package game` — direct access to `session.board` and `session.mu` is valid.** This was raised as a concern at session start ("MoveProcessor cannot access `session.board`"). The concern was wrong: package-level field access means private fields of `GameSession` are directly accessible in `move.go`. The architecture already accounted for this — `move.go` was always planned to be in `internal/game`.

**`Position.Update` is the correct tool for computing next-position FEN without game mutation.** This is not obvious from the notnil/chess documentation (which is sparse). `gopls:go_search` surfaced it. Worth noting in CLAUDE.md as a known pattern.

**Cancelled context is a clean way to simulate DB failure in integration tests.** No store mocking needed. `SaveMove` uses `pgxpool`, which respects context cancellation. Passing a pre-cancelled context causes the DB operation to fail with `context.Canceled` — exactly what is needed to test the persistence-first invariant without mocking infrastructure.

### Problems Encountered

**`Filesystem:write_file` and `Filesystem:edit_file` tool schemas required explicit `tool_search` to load.** Minor friction — tool parameter names were not available until `tool_search` was called. Resolved within the session.

**`bash_tool` runs in Claude's container, not the user's WSL environment.** Cannot run `go test` directly. Tests were run by the user and results reported back. This is expected given the project setup and causes no issues.

No substantive problems encountered.

### Checklist Progress

- ✅ `MoveProcessor` struct with dependencies: `*chess.Validator`, `*store.GameStore`, `*store.MoveStore`, `EventBus`
- ✅ `NewMoveProcessor(...)` constructor
- ✅ `ProcessMove(ctx, session, color, san) error` — full 8-step pipeline
- ✅ Turn validation (wrong turn → `*MoveRejectionError`)
- ✅ Status validation (game not active → `*MoveRejectionError`)
- ✅ `ValidateMove` before DB write (illegal move → `*MoveRejectionError`)
- ✅ `ComputeFENAfterMove` — FEN for DB write without board mutation
- ✅ `MoveStore.SaveMove` on critical path; failure leaves board unchanged
- ✅ `GameStore.UpdateCurrentFEN` after save; failure non-fatal
- ✅ `ApplyMove` under `mu.Lock` after DB write confirms
- ✅ `DetectOutcome` after `ApplyMove`; game-over path handled
- ✅ `GAME_OVER` published via EventBus with outcome/reason/FEN
- ✅ `MOVE_APPLIED` published via EventBus with san/fen/turn/moveNumber/clock values
- ✅ `MoveRejectionError` typed error in `internal/game/errors.go`
- ✅ Integration tests: full pipeline, wrong turn, illegal move, DB failure, checkmate — all passing with `-race`

### Technical Debt Introduced

None this session.

### Files Modified

**Modified:**
- `internal/chess/validator.go` — added `ComputeFENAfterMove`
- `internal/game/errors.go` — added `MoveRejectionError`, added `"fmt"` import

**Created:**
- `internal/game/move.go`
- `internal/game/testmain_integration_test.go`
- `internal/game/move_test.go`

### Recommended Next Step

**Step 9: Clock — implement `internal/game/clock.go`.**

`Clock` struct with fields: `mu sync.Mutex` (protects active, whiteRemaining, blackRemaining, paused, started), `active store.Color`, `whiteRemaining time.Duration`, `blackRemaining time.Duration`, `timer *time.Timer`, `onTimeout func(store.Color)`, `stop chan struct{}`. Public API: `NewClock(initialMs int64) *Clock`, `Start(color Color)`, `Switch()`, `Pause()`, `Resume(color Color)`, `TimeRemaining(color Color) time.Duration`, `SetTimeoutCallback(fn func(Color))`, `Stop()`. The timeout goroutine must drain the timer channel on `Stop` to avoid leaks. All tests must use `goleak.VerifyNone(t)` or manual goroutine count verification to confirm `Stop` terminates the goroutine cleanly. Wire the `Clock` into `GameSession` (add `clock *Clock` field to the protected set) and update `ProcessMove` to call `session.clock.Switch()` after `ApplyMove` and snapshot the updated clock values for `UpdateClocks` and `MOVE_APPLIED`.

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
**Status: 🔄 In Progress — Steps 1–8 Complete, Step 9 (Clock) Next**

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
- [x] MoveProcessor — full 8-step pipeline (Step 8)
- [x] MoveRejectionError — typed client-facing rejection error (Step 8)

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
- [ ] Server-side clock per game (10 minutes per player)
- [ ] Clock starts when both players are connected
- [ ] Clock switches on each move
- [ ] Timeout detection goroutine per game
- [ ] GAME_OVER broadcast on timeout

### Reconnection
- [ ] Player reconnects with playerToken
- [ ] Server maps token to existing GameSession
- [ ] Old connection pointer replaced with new connection
- [ ] Full game state sent to reconnecting player (GAME_STATE message)
- [ ] Opponent notified of reconnection (OPPONENT_RECONNECTED message)

### Persistence Recovery
- [ ] On server restart, active games are recoverable from DB
- [ ] GameSession can be hydrated from DB records
- [ ] In-progress games resume correctly after server restart

### Testing
- [x] Store layer: integration tests with real PostgreSQL (integration build tag)
- [x] Auth layer: unit tests (no build tag, no database)
- [x] Chess layer: unit tests (no build tag, no database)
- [x] Game session and registry: unit tests (no build tag, no database)
- [x] EventBus: unit tests (no build tag, no database)
- [x] Move pipeline: integration tests (integration build tag, real PostgreSQL)
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
| `scanGame` helper | `func(dest ...any) error` parameter | Both `pgx.Row.Scan` and `pgx.Rows.Scan` satisfy this |
| Nullable column scanning | `*string` intermediates, convert to typed pointer | Avoids pgx/v5 reflection path for user-defined string types |
| Store test package | `package store` (internal) | Too many exported domain types to prefix |
| `Color` in PlayerClaims | `string` (not `store.Color`) | Keeps `internal/auth` free of `internal/store` dependency |
| `GetMovesForGame` empty result | `make([]*Move, 0)` (non-nil) | Serializes to `[]` not `null` in JSON |
| `GetActiveGames` empty result | `make([]*Game, 0)` (non-nil) | Same reason |
| `GameOutcome` in `internal/chess` | Separate from `internal/store` types | Keeps chess layer free of store dependency |
| `MoveHistory` uses `g.Moves()` + `g.Positions()` | Not `g.MoveHistory()` | `g.MoveHistory()` panics on nil comments slice (library bug v1.9.0) |
| `DetectOutcome` default draw reason | `"DRAW_AGREEMENT"` | Only valid schema value for non-stalemate draws |
| `GameFromFEN` vs `GameFromMoves` for restart recovery | Both exposed | FEN is O(1) for reconnection; moves for full history accuracy |
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
| `UpdateCurrentFEN` failure in ProcessMove | Non-fatal; logged, pipeline continues | move is in moves table (source of truth); current_fen is a cache |
| `handleGameOver` publishes GAME_OVER even on DB failure | Continues after log | Players must be notified regardless of DB consistency |
| Clock values in MOVE_APPLIED at Step 8 | Pre-move snapshot values | Clock switching deferred to Step 9; not a shortcut, correct sequencing |

---

## Technical Debt

```
TD-001: Player token passed in URL query parameter (visible in logs) | Phase 1 | Fix by: Phase 3
TD-002: Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 | Fix by: Phase 4
TD-003: No draw offer mechanism | Phase 1 | Fix by: Phase 4
TD-004: Anonymous identity only (no real user accounts) | Phase 1 | Fix by: Phase 3
TD-005: Single time control (10+0 only) | Phase 1 | Fix by: Phase 4
TD-006: DetectOutcome maps ThreefoldRepetition/FiftyMoveRule/InsufficientMaterial to "DRAW_AGREEMENT" | Phase 1 | Fix by: Phase 4
TD-007: GameFromFEN loses position history — threefold repetition blind after server restart | Phase 1 | Fix by: Phase 4
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

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://` (for the migrate CLI in the Makefile). When migrations are wired into `main.go` at Step 13, use `pgx5://` scheme with the `golang-migrate/migrate/v4/database/pgx/v5` driver. Different URL schemes, same database — correct and intentional, but will cause confusion at Step 13 if not remembered.

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Any game constructed via `NewGame()` + `MoveStr()` has a nil `comments` field. Do not call `g.MoveHistory()` directly anywhere. Always use the `chess.MoveHistory(g)` wrapper in `internal/chess/validator.go`.

- **`MoveHistory()` returns annotated SAN.** `AlgebraicNotation.Encode` appends `+` for check and `#` for checkmate. The `"moves"` field in `GAME_STATE` messages will contain annotated SAN. Step 8 move pipeline sources move history from `chess.MoveHistory(session.board)` via `CurrentStateSnapshot()`, not from a cached copy of raw client input.

- **`DetectOutcome` must be called after `ApplyMove`.** Calling it before any move is played will always return `(nil, false)` even for checkmate/stalemate positions loaded from FEN.

- **`ReadLoop` context decision deferred to Step 11.** The HTTP request context (`r.Context()`) is cancelled when `ServeHTTP` returns, which is before `ReadLoop` exits. At Step 11, decide whether to pass `r.Context()`, a derived context, or `context.Background()` to `HandleMessage`. The comment in `ReadLoop` flags this explicitly.

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** This is intentional. Do not "optimise" it to snapshot-then-release — that pattern creates a window where `unsubscribe` can close a channel between the snapshot and the send, causing a send-on-closed-channel panic.

- **`ComputeFENAfterMove` must only be called after `ValidateMove` returned nil for the same `(g, san)` pair.** Both use `AlgebraicNotation{}.Decode` internally. If `ValidateMove` passed, `ComputeFENAfterMove` will not error. If it does error, it is a bug — log at Error level and return a plain (non-rejection) error.

- **`move.go` accesses `session.board` and `session.mu` directly.** This is valid because `move.go` is in `package game` — same package as `session.go`. Private fields are accessible within a package. `mu.RLock()` is held for `ValidateMove` + `ComputeFENAfterMove`; `mu.Lock()` is held for `ApplyMove` only.

- **Clock values in `MOVE_APPLIED` are stale until Step 9.** `publishMoveApplied` uses `snap.WhiteTimeMs` and `snap.BlackTimeMs` from the pre-move snapshot. These values are correct but do not reflect clock switching. Step 9 wires in `session.clock.Switch()` after `ApplyMove` and updates the values before publishing.

- **Integration tests for `internal/game` use `//go:build integration`.** Running `go test -race ./internal/game/...` runs only unit tests. Running `go test -race -tags integration ./internal/game/...` runs both unit and integration tests (the `TestMain` in `testmain_integration_test.go` sets up the DB pool; unit tests ignore it).

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

---

## Key Files and Their Responsibilities

```
cmd/server/main.go            — Wires all dependencies, starts HTTP server, handles OS signals
internal/ws/connection.go     — Connection struct, read loop (callback-based), write loop, heartbeat
internal/ws/registry.go       — connID -> *Connection map with RWMutex
internal/ws/errors.go         — ErrConnectionClosed, ErrQueueFull
internal/ws/handler.go        — HTTP upgrade, token validation, GameManager handoff [Step 11]
internal/game/errors.go       — ErrGameNotFound, ErrConnectionOccupied, ErrInvalidTransition, MoveRejectionError
internal/game/messages.go     — All WebSocket message type strings, rejection reasons, error codes
internal/game/session.go      — GameSession struct, state machine, connection management, snapshots
internal/game/registry.go     — gameID -> *GameSession map with RWMutex
internal/game/eventbus.go     — EventBus interface + LocalEventBus implementation
internal/game/manager.go      — Top-level orchestrator: creates sessions, routes messages [Step 10]
internal/game/move.go         — MoveProcessor: 8-step pipeline, MoveRejectionError, handleGameOver
internal/game/clock.go        — Per-game clock, timeout detection goroutine [Step 9]
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

// DetectOutcome: no lock — pure read after exclusive write is complete,
// and only one goroutine calls ProcessMove per session at a time
outcome, ended = validator.DetectOutcome(session.board)
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

## EventBus Key Patterns

**Publish (move pipeline):**
```go
payload, _ := json.Marshal(moveAppliedMsg)
bus.Publish(ctx, game.GameEvent{
    GameID:  session.ID,
    Type:    game.MsgTypeMoveApplied,
    Payload: payload,
})
```

**Subscribe (manager, on game creation):**
```go
ch, unsubscribe, err := bus.Subscribe(ctx, gameID)
defer unsubscribe()  // closes channel, range loop exits cleanly
for event := range ch {
    session.SendToBothPlayers(event.Payload)
}
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

**Step 9: Clock — implement `internal/game/clock.go`.**

`Clock` struct with fields: `mu sync.Mutex` (protects all mutable state), `active store.Color`, `whiteRemaining time.Duration`, `blackRemaining time.Duration`, `paused bool`, `started bool`, `timer *time.Timer`, `onTimeout func(store.Color)`, `stopCh chan struct{}`.

Public API:
- `NewClock(initialMs int64) *Clock` — initialises both sides to `initialMs`, no goroutine started yet
- `SetTimeoutCallback(fn func(store.Color))` — registers the function called when a clock hits zero
- `Start(color store.Color)` — begins countdown for `color`; starts the timeout goroutine
- `Switch()` — stops the active color's clock, records elapsed time, starts the opponent's
- `Pause()` — stops the active clock without switching (called on player disconnect)
- `Resume(color store.Color)` — resumes the paused clock for `color` (called on reconnect)
- `TimeRemaining(color store.Color) time.Duration` — thread-safe read of remaining time
- `Stop()` — signals the timeout goroutine to exit; drains the timer channel to prevent goroutine leak

After `Clock` is complete, wire it into `GameSession`:
- Add `clock *Clock` to the `GameSession` struct (documented in the `mu` protection comment)
- `NewGameSession` initialises `clock` via `NewClock(InitialTimeMs)`
- `ProcessMove` calls `session.clock.Switch()` after `ApplyMove` (between steps 6 and 7), snapshots `session.clock.TimeRemaining` for both colors, calls `session.UpdateClocks(whiteMs, blackMs)` and `gameStore.UpdateClocks(ctx, ...)`, and passes the updated values to `publishMoveApplied`

Unit tests — all must pass with `-race` and verify no goroutine leaks via `goleak.VerifyNone(t)`:
- Clock counts down correctly (Start → wait → TimeRemaining decreased)
- Switch updates the active player and pauses the previous
- Timeout callback fires when clock reaches zero
- Pause/Resume preserves remaining time correctly
- Stop terminates the goroutine cleanly (goleak verification)
- Concurrent reads of TimeRemaining are race-free

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
```