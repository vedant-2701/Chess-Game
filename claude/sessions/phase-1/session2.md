## Session Summary — Migrations, Store Layer, Auth Layer

### What Was Built

**ARCHITECTURE.md correction:**
Added `GET /games/:id` to the Phase 1 endpoint list in `internal/api`, positioned between `POST /games/:id/join` and `GET /health`. This prevents ARCHITECTURE.md from drifting from the PHASE_1.md spec at Step 12.

**Step 2 — Database Migrations (6 files):**
- `000001_create_users.up/down.sql`: `users` table with UUID primary key (no DEFAULT — client supplies the ID)
- `000002_create_games.up/down.sql`: `games` table with status CHECK constraint, two FK references to users, partial index on `status IN ('WAITING_FOR_PLAYER', 'ACTIVE')`
- `000003_create_moves.up/down.sql`: `moves` table with UNIQUE index on `(game_id, move_number)` and explicit `idx_moves_game_id`
- All verified by user: `make migrate-up`, three `make migrate-down` runs, `make migrate-up` again (idempotency confirmed)

**Step 3 — Store Layer (10 files):**
- `internal/store/errors.go`: `ErrGameNotFound`, `ErrUserNotFound` sentinels
- `internal/store/models.go`: All domain types — `GameStatus`, `Color`, `Outcome`, `OutcomeReason`, `GameOutcome`, `User`, `Game`, `Move`, `StartingFEN` constant
- `internal/store/postgres.go`: `NewPool` with ping verification
- `internal/store/user_store.go`: `CreateOrGetUser` (upsert via ON CONFLICT DO UPDATE), `GetUser`
- `internal/store/game_store.go`: `CreateGame`, `GetGame`, `UpdateGameStatus`, `UpdateCurrentFEN`, `UpdatePlayerBlack`, `GetActiveGames`, `UpdateClocks`
- `internal/store/move_store.go`: `SaveMove` (with RETURNING id, played_at), `GetMovesForGame` (ordered by move_number ASC)
- 4 test files: `testmain_test.go`, `user_store_test.go`, `game_store_test.go`, `move_store_test.go` — integration tag, real PostgreSQL, all tests passing with `-race`

**Step 4 — Auth Layer (2 files):**
- `internal/auth/token.go`: `PlayerClaims` struct, `SignPlayerToken` (HS256), `VerifyPlayerToken` with algorithm confusion attack prevention in keyFunc, `ErrTokenExpired` / `ErrTokenInvalid` sentinels
- `internal/auth/token_test.go`: 6 table-driven cases — valid token, expired, wrong secret, tampered signature, malformed string, empty string. No build tag, no database.

### Decisions Made

**`UpdateGameStatus` takes `*GameOutcome` instead of `*Outcome`** — PHASE_1.md spec suggested `*Outcome` but the DB schema enforces that `outcome` and `outcome_reason` are always set together (both NULL or both non-NULL via application convention). A single `*GameOutcome` struct carrying both fields makes the invariant enforced at the type level rather than by convention. No ADR required — this is a function signature correction, not an architectural decision.

**App-generated UUIDs for games** — the game layer will generate the UUID before calling `CreateGame`, so it can sign the JWT with the known gameID immediately. The DB `DEFAULT gen_random_uuid()` remains as a safety fallback but is never relied on. This avoids a RETURNING scan in `CreateGame` and removes the need for a round-trip before signing the JWT.

**`scanGame` uses `func(dest ...any) error` parameter** — both `pgx.Row.Scan` and `pgx.Rows.Scan` satisfy this signature. One helper handles both single-row (`QueryRow`) and multi-row (`Query`) paths without duplicating the column scan order. Column order must exactly match the SELECT list; two copies is a maintenance hazard.

**Store test package is `package store` (internal), not `package store_test` (external)** — addressed as a bug fix mid-session. With this many internal types referenced in tests, the external package would require either a dot-import or prefixing every symbol. No architectural significance.

**`Color` is `string` in `auth.PlayerClaims`** — keeps `internal/auth` free of `internal/store` dependency per the architecture's dependency graph. The game layer validates the color string value after token verification.

**Algorithm confusion prevention in `VerifyPlayerToken`** — keyFunc explicitly rejects any signing method that is not `*jwt.SigningMethodHMAC`. Without this, `alg: none` tokens bypass signature verification entirely.

### Tradeoffs Considered

**`GetMovesForGame` and `GetActiveGames`: nil vs non-nil empty slice** — `var x []*T` is nil, `make([]*T, 0)` is non-nil. Both have zero length but serialize differently in JSON (`null` vs `[]`). The game layer sends move history in `GAME_STATE` messages; a game with no moves yet must send `[]` not `null`. `make([]*Move, 0)` / `make([]*Game, 0)` is required. This was caught as a test failure in `GetActiveGames` after initially using `var games []*Game`.

**Nullable column scanning via `*string` intermediates** — scanning `*Outcome` or `*GameStatus` directly into pgx/v5's reflection path is technically possible but relies on behavior that isn't guaranteed across minor versions for user-defined string types. Scanning as `*string` then converting is two extra lines per nullable field but zero ambiguity. Chosen for correctness over brevity.

**External vs internal test package for store tests** — `package store_test` would have required the `store.` prefix on every type reference, which is verbose but architecturally cleaner (black-box testing). For integration tests of this depth that touch every field of every struct, `package store` is the pragmatic choice. The test binary is still compiled separately; no production binary impact.

### Lessons Learned

**Nil vs empty slice is a test-catching-a-real-bug situation** — `GetActiveGames` returned `nil` when no active games existed. The test asserted `games != nil`. This would have caused the reconnection flow in `RestoreActiveGames` (Step 10) to behave correctly by coincidence (ranging over a nil slice is valid in Go), but the JSON serialization on any endpoint returning a game list would have produced `null` instead of `[]`. The test caught a genuine wire format bug.

**The spec's suggested function signatures are not always complete** — `UpdateGameStatus(ctx, id string, status GameStatus, outcome *Outcome) error` from PHASE_1.md is missing `outcome_reason`. The DB schema makes it obvious both fields must move together, but only because we read the schema carefully. Implementation time is the right time to fix this, as the spec explicitly defers signatures.

**`package store_test` (external) vs `package store` (internal) needs to be a conscious choice made before writing the first test file** — not after a compile error. For packages with many exported domain types used heavily in tests, decide internal vs external upfront.

### Problems Encountered

**Build error: `package store_test` referencing unqualified store types** — all four test files declared `package store_test` but referenced `UserStore`, `Game`, `StartingFEN` etc. without the `store.` prefix. Fixed by changing all four files to `package store`. Root cause: the external test package convention was applied without accounting for how many store types the tests reference directly.

**`GetActiveGames` returning nil empty slice** — `var games []*Game` initializes to nil. Test `expected non-nil empty slice, got nil` caught this. Fixed by user to `games := []*Game{}`. Correct fix is `make([]*Game, 0)` for consistency with `GetMovesForGame`.

**Module path placeholder** — resolved by user (`github.com/vedant-2701/chess`). No longer a sharp edge.

### Checklist Progress

**Step 2: Database Migrations**
- ✅ Migration 001 up/down: `users` table
- ✅ Migration 002 up/down: `games` table with CHECK constraint, two FK refs, partial index
- ✅ Migration 003 up/down: `moves` table with UNIQUE index on (game_id, move_number)
- ✅ `make migrate-up` applies all three cleanly
- ✅ `make migrate-down` rolls back one at a time
- ✅ `make migrate-up` is idempotent

**Step 3: Store Layer**
- ✅ `internal/store/postgres.go`: `NewPool`
- ✅ `internal/store/game_store.go`: all 7 methods
- ✅ `internal/store/move_store.go`: `SaveMove`, `GetMovesForGame`
- ✅ `internal/store/user_store.go`: `CreateOrGetUser`, `GetUser`
- ✅ Integration tests for all store methods passing with `-race`
- ✅ Error wrapping verified: all errors include function name and relevant IDs

**Step 4: Auth Layer**
- ✅ `PlayerClaims` struct: `{ GameID, UserID, Color, RegisteredClaims }`
- ✅ `SignPlayerToken(claims PlayerClaims, secret string) (string, error)`
- ✅ `VerifyPlayerToken(token string, secret string) (*PlayerClaims, error)`
- ✅ Unit test: valid token signs and verifies correctly
- ✅ Unit test: expired token returns `ErrTokenExpired`
- ✅ Unit test: wrong secret returns `ErrTokenInvalid`
- ✅ Unit test: tampered signature returns `ErrTokenInvalid`
- ✅ Unit test: malformed token returns `ErrTokenInvalid`
- ✅ Unit test: empty token returns `ErrTokenInvalid`

### Technical Debt Introduced

None. All three steps are complete with tests passing and no known shortcuts.

### Files Modified

**Created:**
- `migrations/000001_create_users.up.sql`
- `migrations/000001_create_users.down.sql`
- `migrations/000002_create_games.up.sql`
- `migrations/000002_create_games.down.sql`
- `migrations/000003_create_moves.up.sql`
- `migrations/000003_create_moves.down.sql`
- `internal/store/errors.go`
- `internal/store/models.go`
- `internal/store/postgres.go`
- `internal/store/user_store.go`
- `internal/store/game_store.go`
- `internal/store/move_store.go`
- `internal/store/testmain_test.go`
- `internal/store/user_store_test.go`
- `internal/store/game_store_test.go`
- `internal/store/move_store_test.go`
- `internal/auth/token.go`
- `internal/auth/token_test.go`

**Modified:**
- `ARCHITECTURE.md` — `GET /games/:id` added to Phase 1 endpoint list (user action)
- `CLAUDE.md` — `turn` field casing corrected to uppercase (user action, session start)

### Recommended Next Step

**Step 5: Chess Layer** — implement `internal/chess/validator.go` wrapping `notnil/chess`. Four operations: `ValidateAndApply`, `DetectOutcome`, `CurrentFEN`, `MoveHistory`. Plus `GameFromFEN` and `GameFromMoves` for state reconstruction. Unit tests must cover: valid move application, illegal move rejection, Scholar's mate (checkmate detection), stalemate detection, en passant, castling, FEN round-trip. No database, no integration tag. Estimated 1–2 hours. This is the last foundational layer before the game session and move pipeline (Steps 6–8), which depend on it.

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
**Status: 🔄 In Progress — Steps 1–4 Complete, Step 5 (Chess Layer) Next**

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
- [ ] notnil/chess integration
- [ ] Move validation wrapper (ValidateAndApply)
- [ ] Game result detection (DetectOutcome)
- [ ] FEN extraction (CurrentFEN)
- [ ] SAN move recording (MoveHistory)
- [ ] GameFromFEN, GameFromMoves for state reconstruction

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

---

## Technical Debt

Pre-declared in PHASE_1.md (will be introduced during implementation):

```
TD-001: Player token passed in URL query parameter (visible in logs) | Phase 1 | Fix by: Phase 3
TD-002: Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 | Fix by: Phase 4
TD-003: No draw offer mechanism | Phase 1 | Fix by: Phase 4
TD-004: Anonymous identity only (no real user accounts) | Phase 1 | Fix by: Phase 3
TD-005: Single time control (10+0 only) | Phase 1 | Fix by: Phase 4
```

No additional debt introduced in implemented steps so far.

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

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://` (for the migrate CLI in the Makefile). When migrations are wired into `main.go` at Step 13, use `pgx5://` scheme with the `golang-migrate/migrate/v4/database/pgx/v5` driver. Different URL schemes, same database — correct and intentional, but will cause confusion at Step 13 if not remembered.

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
internal/chess/validator.go   — Wraps notnil/chess: ValidateAndApply, DetectOutcome, CurrentFEN, MoveHistory
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

**Step 5: Chess Layer**

Implement `internal/chess/validator.go` wrapping the `notnil/chess` library.

Functions to implement:
- `NewValidator() *Validator`
- `NewGame() *chess.Game` — starting position
- `GameFromFEN(fen string) (*chess.Game, error)` — reconstruct from FEN string
- `GameFromMoves(moves []string) (*chess.Game, error)` — replay move history from SAN slice
- `ValidateAndApply(game *chess.Game, san string) (*chess.Game, error)` — returns new state, never mutates input
- `DetectOutcome(game *chess.Game) (outcome, reason, hasOutcome)` — exact return shape decided at implementation
- `CurrentFEN(game *chess.Game) string`
- `MoveHistory(game *chess.Game) []string`

Unit tests required (no build tag, no database):
- Valid move applies and returns updated game state
- Illegal move (wrong turn) returns error, input game unchanged
- Illegal move (invalid piece movement) returns error
- Scholar's mate position → checkmate detected
- Stalemate position → stalemate detected
- En passant validates correctly
- Castling validates correctly
- FEN round-trip: `GameFromFEN(fen)` → `CurrentFEN()` returns original FEN

---

## Session Log

| Session | Date | What Was Done |
|---------|------|---------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only: removed CONNECT from WS protocol; fixed time fields to whiteTimeMs/blackTimeMs; replaced chess.Validator Go interface with behavioral descriptions; added UserStore to Step 3 checklist; replaced DetectOutcome signature with behavioral description; removed prometheus from Phase 1 deps |
| 3 | 2025-01-XX | Step 1 scaffold complete: go.mod (module path github.com/vedant-2701/chess), docker-compose.yml, .env.example, Makefile (15 targets), all 8 package directories |
| 4 | 2025-01-XX | turn field casing corrected to uppercase in CLAUDE.md. ARCHITECTURE.md updated with GET /games/:id. Step 2 (migrations) complete: 6 files, all verified. Step 3 (store layer) complete: 6 implementation files + 4 integration test files, all tests passing with -race. Fixed nil-vs-empty-slice bug in GetActiveGames. Fixed package store_test → package store build error. Step 4 (auth layer) complete: token.go + token_test.go, 6 unit tests passing with -race. |
```