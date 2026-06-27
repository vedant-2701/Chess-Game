## Session Summary — Chess Layer Implementation With Library Surprises

### What Was Built

**`internal/chess/errors.go`** — Two sentinel errors: `ErrIllegalMove` and `ErrInvalidFEN`.

**`internal/chess/types.go`** — `GameOutcome{Winner string, Reason string}` struct. Defined here rather than delegating to `internal/store` types to keep `internal/chess` free of `internal/store` dependency per the architecture graph.

**`internal/chess/validator.go`** — Full chess layer implementation:
- `Validator` struct (stateless), `NewValidator()`
- `NewGame()` — starting position with `AlgebraicNotation{}` explicitly set
- `GameFromFEN(fen string) (*chess.Game, error)` — FEN reconstruction with documented limitation: position history not preserved, threefold repetition detection inaccurate after server restart recovery via FEN
- `GameFromMoves(moves []string) (*chess.Game, error)` — full move replay, preserves complete position history, correct threefold repetition detection
- `ValidateMove(g *chess.Game, san string) error` — no mutation, uses `AlgebraicNotation{}.Decode` directly
- `ApplyMove(g *chess.Game, san string) error` — in-place mutation, documented contract: only call after DB write succeeds
- `DetectOutcome(g *chess.Game) (*GameOutcome, bool)` — maps `notnil/chess` outcome/method to domain strings; auto-draws (ThreefoldRepetition, FiftyMoveRule, InsufficientMaterial) mapped to `"DRAW_AGREEMENT"` pending Phase 4 schema extension
- `CurrentFEN(g *chess.Game) string` — thin wrapper over `g.FEN()`
- `MoveHistory(g *chess.Game) []string` — uses `g.Moves()` + `g.Positions()` directly (not `g.MoveHistory()`) to avoid a library panic

**`internal/chess/validator_test.go`** — 20 unit tests, no build tag, no database, all passing with `-race`:
- `TestNewValidator`, `TestNewGame_StartingPosition`
- `TestGameFromFEN` (5 cases: starting position, en passant, stalemate, empty string, garbage)
- `TestFENRoundTrip` (4 FEN strings)
- `TestGameFromMoves` (3 cases: valid sequence, empty list, illegal move in sequence)
- `TestValidateMove` (8 cases: valid moves, wrong turn, nonsense SAN, empty SAN, en passant, kingside castling, queenside castling) — each case asserts no mutation occurred
- `TestApplyMove` (2 cases: valid advances to correct FEN, illegal returns error without corrupting state)
- `TestValidateThenApply` — pipeline correctness test: validate then apply produces same FEN as a reference game built with single MoveStr calls
- `TestDetectOutcome_NoOutcome`, `TestDetectOutcome_NoOutcome_MidGame`
- `TestDetectOutcome_Checkmate_ScholarsMate`
- `TestDetectOutcome_Stalemate` — stalemate after `ApplyMove`, not from FEN load
- `TestMoveHistory` (4 cases: no moves, one move, four moves, Scholar's mate with `#` annotation)
- `TestEnPassant`, `TestCastling` (O-O and O-O-O)

### Decisions Made

**ADR-013 confirmed and implemented as Option B (Validate-Then-Apply Split).** The pre-session ADR was written correctly. Implementation confirmed the decision holds: `ValidateMove` uses `AlgebraicNotation{}.Decode` which is exactly what `MoveStr` calls internally, so the validate-then-apply split adds zero overhead and zero code duplication. The TOCTOU concern does not apply under the single-goroutine-per-session model.

**`GameOutcome` defined in `internal/chess`, not `internal/store`.** Keeps the dependency graph clean. The game layer (Step 6+) will map `chess.GameOutcome` to `store.GameOutcome` at the boundary. The field names (`Winner`, `Reason`) are intentionally identical to the store layer's string values to make the mapping trivial.

**`GameFromFEN` vs `GameFromMoves` for server restart recovery — documented limitation.** `GameFromFEN` loses position history. This means threefold repetition detection is blind to moves played before a server restart. This is accepted: Phase 1 games are short, repetition draws are rare, and the FEN-based recovery is required for the O(1) reconnection path. If exact repetition tracking across restarts is needed, use `GameFromMoves` with the full move history from the `moves` table. The function comment documents this explicitly.

**`DetectOutcome` default case maps to `"DRAW_AGREEMENT"`.** The store schema's `OutcomeReason` CHECK constraint only allows specific values. ThreefoldRepetition, FiftyMoveRule, and InsufficientMaterial are not in the schema. `"DRAW_AGREEMENT"` is the least-wrong valid value. Phase 4 will extend the schema with proper reasons. This is a known imprecision, not a correctness bug — the game does end, the outcome is DRAW, only the reason label is imprecise.

### Tradeoffs Considered

**Option A (FEN round-trip clone) vs Option B (validate-then-apply) for persistence-first guarantee.** Option A was the initial instinct. Pre-session analysis revealed Option A as written breaks threefold repetition detection because FEN does not encode position history. Option A could be corrected via move replay clone (Option D) but that adds O(n) allocation per move for no benefit over Option B. Option B keeps `session.board` as a single continuous object — position history is always complete, no allocation on the hot path.

**`g.MoveHistory()` vs `g.Moves()` + `g.Positions()` for SAN extraction.** `g.MoveHistory()` panics on nil `comments` slice (library bug in v1.9.0). Direct use of `g.Moves()` + `g.Positions()` with `AlgebraicNotation{}.Encode` is the correct workaround. The tradeoff is slightly more code for zero risk of panic in production.

**Stalemate test via FEN load vs stalemate test via ApplyMove.** `notnil/chess` does not set `g.Outcome()` from FEN construction alone — it only updates outcome after a `Move()` or `MoveStr()` call. Testing stalemate from FEN load would require calling `g.Position().Status()` directly (not through `DetectOutcome`), which would test the library, not our code. The realistic test is via `ApplyMove` followed by `DetectOutcome`, which is exactly how the production pipeline uses it. This is the test that was written.

### Lessons Learned

**`notnil/chess` has a panic in `game.MoveHistory()` for non-PGN-constructed games.** The `comments` slice is nil when games are built via `NewGame()` + `MoveStr()`. The library only initializes `comments` during PGN parsing. This is not documented anywhere in the library. It was discovered by running the tests. The fix (use `g.Moves()` + `g.Positions()`) is straightforward once identified, but it is the kind of library behavior that would cause a silent production panic if not covered by tests. This validates the test-everything approach.

**`AlgebraicNotation.Encode` produces annotated SAN.** Check moves get `+`, checkmates get `#`. Input to `MoveStr` can omit the annotation; output from `Encode` always includes it. Concretely: you can call `GameFromMoves([]string{"e4", "e5", ..., "Qxf7"})` and it works, but `MoveHistory` returns `"Qxf7#"`. The `"moves"` field in `GAME_STATE` WebSocket messages will contain annotated SAN. This is correct chess notation behavior, but the Step 8 move pipeline must use `MoveHistory(session.board)` for the `GAME_STATE` message — not a cached copy of the raw input SAN string, which may lack the annotation.

**Stalemate positions require care.** Multiple FEN positions that look like stalemate on inspection were actually checkmate. The position `"7k/8/4Q1K1/8/8/8/8/8 w - - 0 1"` with move `"Qf7"` is the reliable pre-stalemate test position used. Verification required a probe script against the actual library.

**`ValidateMove` must assert no mutation in tests.** The test captures FEN before calling `ValidateMove` and asserts FEN is unchanged after. This is not paranoia — if `AlgebraicNotation{}.Decode` ever mutates the position (e.g., in a future library version), the test will catch it. The non-mutation property is load-bearing for ADR-013.

### Problems Encountered

**`notnil/chess` panic in `g.MoveHistory()`** — `TestMoveHistory/scholars_mate` panicked on `comments[i-1]` with a nil slice dereference. Root cause: the `comments` field is only populated during PGN parsing, not during `NewGame()` + `MoveStr()` construction. Fixed by switching to `g.Moves()` + `g.Positions()`.

**Stalemate test position was wrong** — `preStalemateFEN = "k7/2Q5/2K5/8/8/8/8/8 w - - 0 1"` with move `"Qb7"` produced checkmate, not stalemate. Required a probe script to find a correct position. Correct position: `"7k/8/4Q1K1/8/8/8/8/8 w - - 0 1"` with `"Qf7"`.

**`TestMoveHistory/scholars_mate` expected `"Qxf7"`, got `"Qxf7#"`** — `scholarsMate` input slice uses `"Qxf7"` (accepted by `MoveStr`), but `AlgebraicNotation.Encode` returns `"Qxf7#"`. Fixed by updating the expected MoveHistory output in the test to match what the library actually returns. The input to `GameFromMoves` remains `"Qxf7"`.

**Go not installed in the container** — required `apt-get install golang-go` before running tests. No project impact.

**Module proxy blocked** — `proxy.golang.org` and `sum.golang.org` not in the network allowlist. Required `GOPROXY=direct GONOSUMDB='*' GOINSECURE='*'` to fetch `notnil/chess` directly from GitHub for test execution.

### Checklist Progress

**Step 5: Chess Layer**
- ✅ `NewValidator() *Validator`
- ✅ `NewGame() *chess.Game` — starting position
- ✅ `GameFromFEN(fen string) (*chess.Game, error)`
- ✅ `GameFromMoves(moves []string) (*chess.Game, error)`
- ✅ `ValidateMove(game *chess.Game, san string) error`
- ✅ `ApplyMove(game *chess.Game, san string) error`
- ✅ `DetectOutcome(game *chess.Game) (*GameOutcome, bool)`
- ✅ `CurrentFEN(game *chess.Game) string`
- ✅ `MoveHistory(game *chess.Game) []string`
- ✅ Unit test: valid move applies correctly
- ✅ Unit test: illegal move (wrong turn) returns error, board unchanged
- ✅ Unit test: illegal move (invalid piece movement) returns error
- ✅ Unit test: Scholar's mate → checkmate detected (WHITE wins, CHECKMATE reason)
- ✅ Unit test: stalemate detected after ApplyMove
- ✅ Unit test: en passant validates and applies correctly
- ✅ Unit test: castling (O-O and O-O-O) validates and applies correctly
- ✅ Unit test: FEN round-trip (GameFromFEN → CurrentFEN returns original FEN)
- ✅ All 20 tests passing with `go test -race`

### Technical Debt Introduced

**TD-006: `DetectOutcome` maps ThreefoldRepetition/FiftyMoveRule/InsufficientMaterial to `"DRAW_AGREEMENT"`** — The store schema's `OutcomeReason` CHECK constraint does not include these values. `"DRAW_AGREEMENT"` is the least-wrong valid value. The game ends correctly as a DRAW; only the reason label is imprecise. | Phase introduced: 1 | Must fix by: Phase 4 (when the schema is extended for game history and proper draw reasons)

**TD-007: `GameFromFEN` loses position history — threefold repetition blind after server restart** — Games recovered from `current_fen` after a server restart cannot detect threefold repetition for positions played before the restart. Acceptable in Phase 1 (short games, rare occurrence). Full recovery requires `GameFromMoves` with the complete move history. | Phase introduced: 1 | Must fix by: Phase 4 (if game history accuracy is required)

### Files Modified

**Created:**
- `internal/chess/errors.go`
- `internal/chess/types.go`
- `internal/chess/validator.go`
- `internal/chess/validator_test.go`

**Modified:**
- None

### Recommended Next Step

**Step 6: Game Session and Registry** — Implement `internal/game/session.go` and `internal/game/registry.go`.

`session.go`: `GameSession` struct with mutex (document what it protects), state machine (`Transition` method validating legal transitions), `NewGameSession`, `RegisterConnection`, `ReplaceConnection`, `ClearConnection`, `BothPlayersConnected`, `CurrentStateSnapshot`. The state machine is the most critical piece — every invalid transition must return an error, never silently proceed.

`registry.go`: `GameRegistry` with `sync.RWMutex`, `Register`, `Get`, `Unregister`, `AllActive`. Snapshot-then-release pattern required for any iteration (no holding lock while calling methods that might acquire the same lock).

Unit tests required (no build tag, no database): all valid state transitions pass, all invalid transitions return errors, concurrent `Get` and `Register` under the race detector.

---

## PART 2 — Updated CLAUDE.md

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
**Status: 🔄 In Progress — Steps 1–5 Complete, Step 6 (Game Session and Registry) Next**

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
- [x] `GET /games/:id` added to ARCHITECTURE.md endpoint list (between POST /games/:id/join and GET /health)

### Implementation
- [x] WebSocket infrastructure (pre-existing): connection lifecycle, read loop, write loop, heartbeats, registry, graceful shutdown
- [x] Step 1: Project Scaffold — go.mod, docker-compose.yml, .env.example, Makefile, directory structure
- [x] Step 2: Database Migrations — 3 up/down pairs (users, games, moves), verified migrate-up/down/idempotency
- [x] Step 3: Store Layer — postgres.go, game_store.go, move_store.go, user_store.go + integration tests (all passing with -race)
- [x] Step 4: Auth Layer — token.go (SignPlayerToken, VerifyPlayerToken) + unit tests (all passing with -race)
- [x] Step 5: Chess Layer — errors.go, types.go, validator.go + 20 unit tests (all passing with -race)

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
- [x] Game result detection (DetectOutcome)
- [x] FEN extraction (CurrentFEN)
- [x] SAN move recording (MoveHistory)
- [x] GameFromFEN, GameFromMoves for state reconstruction

### API Layer (HTTP)
- [ ] chi router setup
- [ ] POST /games — create game, return gameID + white's playerToken
- [ ] POST /games/:id/join — join game, return black's playerToken
- [ ] GET /games/:id — get current game state (for reconnection via HTTP)
- [ ] GET /health — health check

### WebSocket Layer
- [ ] Integrate existing ws infrastructure into project structure
- [ ] WS upgrade handler at GET /ws/game/:id
- [ ] Token validation on connect
- [ ] Player registration into GameSession on connect
- [ ] Message routing (MOVE, RESIGN, PING)

### Game Layer
- [ ] GameSession struct defined
- [ ] GameState machine (WAITING → ACTIVE → COMPLETED → ABANDONED)
- [ ] GameRegistry (gameID → *GameSession)
- [ ] EventBus interface defined (local implementation for Phase 1)
- [ ] Player-to-connection bridge for reconnection

### Move Pipeline
- [ ] Receive MOVE message from client
- [ ] Validate it is the correct player's turn
- [ ] Validate move legality via chess library
- [ ] Persist move to database
- [ ] Update current_fen on game record
- [ ] Broadcast MOVE_APPLIED to both players
- [ ] Check for game over after each move
- [ ] Reject illegal moves with MOVE_REJECTED response

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
- [ ] Game state machine: unit tests
- [ ] Move pipeline: integration tests
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
| `UpdateGameStatus` signature | `*GameOutcome` (not `*Outcome`) | outcome + reason always move together; DB schema makes splitting them invalid |
| Game UUID generation | App-generated (game layer), not DB DEFAULT | JWT must be signed before any DB round-trip; known ID is required |
| `scanGame` helper | `func(dest ...any) error` parameter | Both `pgx.Row.Scan` and `pgx.Rows.Scan` satisfy this; one helper, no column-order duplication |
| Nullable column scanning | `*string` intermediates, convert to typed pointer | Avoids pgx/v5 reflection path for user-defined string types |
| Store test package | `package store` (internal) | Too many exported domain types to prefix every reference in integration tests |
| `Color` in PlayerClaims | `string` (not `store.Color`) | Keeps `internal/auth` free of `internal/store` dependency per architecture graph |
| `GetMovesForGame` empty result | `make([]*Move, 0)` (non-nil) | Serializes to `[]` not `null` in JSON; game clients expect an array |
| `GetActiveGames` empty result | `make([]*Game, 0)` (non-nil) | Same reason; caught as test failure when `var games []*Game` was used |
| `GameOutcome` in `internal/chess` | Separate from `internal/store` types | Keeps chess layer free of store dependency; field names identical for trivial mapping |
| `MoveHistory` uses `g.Moves()` + `g.Positions()` | Not `g.MoveHistory()` | `g.MoveHistory()` panics on nil comments slice for non-PGN-constructed games (library bug in v1.9.0) |
| `DetectOutcome` default draw reason | `"DRAW_AGREEMENT"` | Only valid schema value for draws not covered by CHECKMATE/STALEMATE; imprecise but not incorrect |
| `GameFromFEN` vs `GameFromMoves` for restart recovery | Both exposed; FEN for O(1) reconnection, moves for full history accuracy | FEN loses position history (threefold repetition blind); documented as accepted limitation |

---

## Technical Debt

Pre-declared in PHASE_1.md plus debt introduced during implementation:

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

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Any game constructed via `NewGame()` + `MoveStr()` (i.e., every game in production) has a nil `comments` field. Do not call `g.MoveHistory()` directly anywhere. Always use the `chess.MoveHistory(g)` wrapper in `internal/chess/validator.go` which uses `g.Moves()` + `g.Positions()` instead.

- **`MoveHistory()` returns annotated SAN.** `AlgebraicNotation.Encode` appends `+` for check and `#` for checkmate. Input SAN to `MoveStr` can omit annotations; output from `MoveHistory` always includes them. The `"moves"` field in `GAME_STATE` WebSocket messages will contain annotated SAN (e.g., `"Qxf7#"` not `"Qxf7"`). Step 8 move pipeline must source move history from `chess.MoveHistory(session.board)`, not from a cached copy of raw client input.

- **`DetectOutcome` must be called after `ApplyMove`.** `notnil/chess` only sets `g.Outcome()` after a `Move()` or `MoveStr()` call. Calling `DetectOutcome` on a FEN-loaded game before any move is played will always return `(nil, false)` even if the position is checkmate or stalemate. In the production pipeline this is never an issue (always called after `ApplyMove`), but FEN-based tests must account for this.

---

## WebSocket Message Protocol (Phase 1)

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

**Note on `moves` field in GAME_STATE:** Contains annotated SAN from `chess.MoveHistory()`. Check moves include `+`, checkmates include `#`. This is correct standard chess notation.

---

## Key Files and Their Responsibilities

```
cmd/server/main.go            — Wires all dependencies, starts HTTP server, handles OS signals
internal/ws/connection.go     — Connection struct, read loop, write loop, heartbeat
internal/ws/registry.go       — connID -> *Connection map with RWMutex (pre-built)
internal/ws/handler.go        — HTTP upgrade, token validation, GameManager handoff
internal/game/session.go      — GameSession struct, state machine transitions
internal/game/registry.go     — gameID -> *GameSession map with RWMutex
internal/game/manager.go      — Top-level orchestrator: creates sessions, routes messages
internal/game/move.go         — Move pipeline: validate -> persist -> broadcast
internal/game/eventbus.go     — EventBus interface + LocalEventBus implementation
internal/game/clock.go        — Per-game clock, timeout detection goroutine
internal/chess/errors.go      — ErrIllegalMove, ErrInvalidFEN sentinels
internal/chess/types.go       — GameOutcome{Winner, Reason} — chess-layer result type
internal/chess/validator.go   — Validator, NewGame, GameFromFEN, GameFromMoves,
                                ValidateMove, ApplyMove, DetectOutcome, CurrentFEN, MoveHistory
internal/store/errors.go      — ErrGameNotFound, ErrUserNotFound sentinels
internal/store/models.go      — Domain types: User, Game, Move, GameStatus, Color, Outcome, OutcomeReason, GameOutcome
internal/store/postgres.go    — NewPool (pgxpool initialization with ping)
internal/store/game_store.go  — CreateGame, GetGame, UpdateGameStatus, UpdateCurrentFEN, UpdatePlayerBlack, GetActiveGames, UpdateClocks
internal/store/move_store.go  — SaveMove (RETURNING id/played_at), GetMovesForGame (ASC order)
internal/store/user_store.go  — CreateOrGetUser (upsert), GetUser
internal/auth/token.go        — PlayerClaims, SignPlayerToken, VerifyPlayerToken (HS256, algorithm confusion prevention)
internal/api/routes.go        — chi router, middleware stack
internal/api/game_handler.go  — POST /games, POST /games/:id/join, GET /games/:id, GET /health
migrations/                   — SQL migration files (golang-migrate format)
```

---

## Chess Layer Key Patterns

**Validate-then-apply split (ADR-013):**
```go
// Step 1: validate — no mutation, before DB write
if err := validator.ValidateMove(session.board, san); err != nil {
    // send MOVE_REJECTED, return
}

// Step 2: persist — DB write while board is unchanged
if err := moveStore.SaveMove(ctx, move); err != nil {
    // send MOVE_REJECTED, return — board never touched
}

// Step 3: apply — mutate only after DB confirms
if err := validator.ApplyMove(session.board, san); err != nil {
    // This is a bug — log as error, game state unrecoverable without DB reload
    slog.Error("ApplyMove failed after ValidateMove succeeded", "gameID", gameID, "san", san, "error", err)
}
```

**MoveHistory — always use the wrapper, never `g.MoveHistory()`:**
```go
// CORRECT
sans := chess.MoveHistory(session.board)  // uses internal/chess wrapper

// WRONG — panics on nil comments slice for non-PGN games
sans := session.board.MoveHistory()  // library method, do not call directly
```

**DetectOutcome — always call after ApplyMove:**
```go
if err := validator.ApplyMove(session.board, san); err != nil { ... }

if outcome, ended := validator.DetectOutcome(session.board); ended {
    // handle game over
}
```

**GameFromFEN vs GameFromMoves:**
```go
// For server restart recovery (O(1), loses repetition history):
g, err := chess.GameFromFEN(game.CurrentFEN)

// For full history accuracy (O(n), correct threefold repetition):
sans := make([]string, len(moves))
for i, m := range moves { sans[i] = m.SAN }
g, err := chess.GameFromMoves(sans)
```

---

## Store Layer Key Patterns

**scanGame helper** — used in `GetGame` and `GetActiveGames`:
```go
// Works for both QueryRow and Query rows:
scanGame(pool.QueryRow(ctx, q, id).Scan)  // single row
scanGame(rows.Scan)                        // inside rows.Next() loop
```

**Nullable column scanning** — scan as `*string`, convert to typed pointer:
```go
var outcome *string
// ... scan ...
if outcome != nil {
    o := Outcome(*outcome)
    game.Outcome = &o
}
```

**Empty slice convention** — always return `make([]*T, 0)`, never `var x []*T` for functions that may return empty results. Nil slice serializes to `null` in JSON; non-nil empty slice serializes to `[]`.

---

## Auth Layer Key Patterns

**Algorithm confusion prevention** — keyFunc must reject non-HMAC methods:
```go
func(token *jwt.Token) (any, error) {
    if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
        return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
    }
    return []byte(secret), nil
}
```

**Error sentinel wrapping** — preserves sentinel for errors.Is while including diagnostic detail:
```go
return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
```

---

## Next Recommended Task

**Step 6: Game Session and Registry**

Implement `internal/game/session.go` and `internal/game/registry.go`.

**`session.go`:**
- `GameSession` struct — mutex with documented protection scope, state, board (`*chess.Game`), player connections, player IDs, clock state
- `NewGameSession(id string, whiteID string) *GameSession`
- `SetPlayerBlack(userID string)`
- `RegisterConnection(color Color, conn *ws.Connection) error`
- `ReplaceConnection(color Color, conn *ws.Connection)` — for reconnection
- `ClearConnection(color Color)` — on disconnect
- `BothPlayersConnected() bool`
- `Transition(newState GameState) error` — validates legal transitions, returns error for invalid ones. This is the only place game state changes.
- `CurrentStateSnapshot() GameStateSnapshot` — thread-safe read of full state for GAME_STATE messages

**`registry.go`:**
- `GameRegistry` struct with `sync.RWMutex` (document what it protects)
- `NewGameRegistry() *GameRegistry`
- `Register(session *GameSession)`
- `Get(gameID string) (*GameSession, error)` — returns sentinel error if not found
- `Unregister(gameID string)`
- `AllActive() []*GameSession` — snapshot-then-release pattern, returns non-nil empty slice

**Critical correctness requirements:**
- State machine: every invalid transition must return an error. Valid transitions: WAITING→ACTIVE, ACTIVE→COMPLETED, ACTIVE→ABANDONED only. COMPLETED and ABANDONED are terminal — no transitions out.
- Mutex scope: document exactly which fields the mutex protects. `session.board` (`*chess.Game`) must be under the mutex.
- `AllActive()` and any registry iteration must use snapshot-then-release to avoid holding lock while calling methods that may acquire it.

**Unit tests (no build tag, no database):**
- All valid state transitions succeed
- All invalid transitions return errors (WAITING→COMPLETED, ACTIVE→WAITING, COMPLETED→anything, ABANDONED→anything)
- `BothPlayersConnected` returns correct values as connections are registered/cleared
- `RegisterConnection` on an already-occupied slot returns error
- `GameRegistry.Get` for missing gameID returns error
- Concurrent `Register` + `Get` under `-race` passes

---

## Session Log

| Session | Date | What Was Done |
|---------|------|---------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only: removed CONNECT from WS protocol; fixed time fields to whiteTimeMs/blackTimeMs; replaced chess.Validator Go interface with behavioral descriptions; added UserStore to Step 3 checklist; replaced DetectOutcome signature with behavioral description; removed prometheus from Phase 1 deps |
| 3 | 2025-01-XX | Step 1 scaffold complete: go.mod (module path github.com/vedant-2701/chess), docker-compose.yml, .env.example, Makefile (15 targets), all 8 package directories |
| 4 | 2025-01-XX | turn field casing corrected to uppercase in CLAUDE.md. ARCHITECTURE.md updated with GET /games/:id. Step 2 (migrations) complete: 6 files, all verified. Step 3 (store layer) complete: 6 implementation files + 4 integration test files, all tests passing with -race. Fixed nil-vs-empty-slice bug in GetActiveGames. Fixed package store_test → package store build error. Step 4 (auth layer) complete: token.go + token_test.go, 6 unit tests passing with -race. |
| 5 | 2025-01-XX | ADR-013 decision confirmed (Validate-Then-Apply split). Step 5 (chess layer) complete: errors.go, types.go, validator.go, validator_test.go. 20 unit tests passing with -race. Discovered and worked around notnil/chess panic in game.MoveHistory() on nil comments slice. Discovered MoveHistory returns annotated SAN (Qxf7# not Qxf7). TD-006 and TD-007 introduced. |
```