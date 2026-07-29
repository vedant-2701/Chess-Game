## Session Summary — Documentation Corrections and Project Scaffold

### What Was Built

**Documentation corrections (no implementation):**
- Removed the `CONNECT` message from the WebSocket Client→Server protocol in CLAUDE.md. Authentication is URL query-param only (`?token=<jwt>`); there is no post-connect auth message.
- Fixed time field names and units in CLAUDE.md protocol samples: `whiteTime`/`blackTime` (ambiguous unit) → `whiteTimeMs`/`blackTimeMs` (explicit milliseconds), values updated to match PHASE_1.md (`598000`, `597843`, `600000`).
- Replaced the Go interface code block for `chess.Validator` in ARCHITECTURE.md with four behavioral bullet-point descriptions. The old block had `DetectOutcome` returning `(Outcome, bool)` — two values — which conflicts with the three-value return needed in practice (winner + reason + whether game is over). Removing the signature defers the exact return shape to implementation time.
- Added `UserStore` block to PHASE_1.md Step 3 checklist (`CreateOrGetUser`, `GetUser`). The `POST /games` handler internally upserts a user record — this store method was absent from the checklist, which would have caused a backtrack at Step 12.
- Replaced the `DetectOutcome` Go signature in PHASE_1.md Step 5 with a behavioral description. The old signature `(outcome Outcome, reason OutcomeReason, hasOutcome bool)` was in conflict with ARCHITECTURE.md's `(Outcome, bool)`.
- Removed `github.com/prometheus/client_golang` from the PHASE_1.md Step 1 dependency list. Prometheus is a Phase 6 concern; including it in Phase 1 is scope creep and contradicts the explicit out-of-scope discipline the project is built on.

**Project scaffold (Step 1):**
- `go.mod` — module path `github.com/yourusername/chess-server`, Go 1.22, all seven direct dependencies at pinned versions, plus `github.com/uber-go/goleak` for goroutine leak tests (required by acceptance criterion #7).
- `docker-compose.yml` — PostgreSQL 16 with healthcheck (`pg_isready`), named volume for data persistence, Redis service block fully commented out with a note to uncomment in Phase 2.
- `.env.example` — four variables: `SERVER_PORT`, `LOG_LEVEL`, `DATABASE_URL` (matching docker-compose credentials), `JWT_SECRET` with generation instruction.
- `Makefile` — all eight required targets (`run`, `build`, `test`, `test-race`, `migrate-up`, `migrate-down`, `docker-up`, `docker-down`, `lint`) plus `test-integration`, `vet`, `tidy`, `install-tools`, `docker-reset`, `migrate-drop`, `clean`, and `help`. Uses `-include .env` + `export` so `DATABASE_URL` is available to migrate targets without manual export. Tool versions pinned as Makefile variables.
- Directory structure — all eight package directories created: `cmd/server/`, `internal/api/`, `internal/auth/`, `internal/chess/`, `internal/game/`, `internal/store/`, `internal/ws/`, `migrations/`.

### Decisions Made

No new architectural decisions were made. All decisions this session were corrections to existing documentation to make it internally consistent. No ADRs required.

### Tradeoffs Considered

**migrate CLI vs. Go-based migration runner in Makefile targets:** The Makefile uses the standalone `migrate` CLI binary for `migrate-up` and `migrate-down` targets. An alternative is a small `cmd/migrate/main.go` that uses the `golang-migrate` Go library directly, keeping everything self-contained in the module. The CLI approach was chosen because the Go library's migration runner at startup (Step 13) and the CLI for development can coexist cleanly, and the CLI is the standard developer tool for migration operations. The tradeoff is that `make install-tools` must be run before migrate targets work.

**`migrate` CLI URL scheme vs. pgx/v5 scheme:** The `.env.example` uses `postgres://` scheme, which the `migrate` CLI handles with its own postgres driver (lib/pq internally). The Go application at startup will use the `pgx5://` scheme with `golang-migrate/migrate/v4/database/pgx/v5`. This means the Makefile targets and the Go startup code use different URL schemes pointing to the same database. This is correct and intentional — it is not a bug — but it is a sharp edge that will cause confusion at Step 13 if not remembered.

### Lessons Learned

The documentation review before writing any code caught five real defects that would have caused implementation problems: a missing store method (UserStore) that would have required backtracking from Step 12 to Step 3; a conflicting return signature for `DetectOutcome` that would have caused a compiler error when integrating the chess and game layers; a wrong protocol message (CONNECT) that would have caused the WebSocket handler to handle a message type the spec does not define; wrong time field names/units that would have produced incorrect wire format; and an out-of-scope dependency that adds dead weight. The session start ritual is not bureaucracy — it caught all five before a line of Go was written.

### Problems Encountered

Go is not installed in the build environment. `go mod tidy` cannot be run to populate indirect dependencies and generate `go.sum`. The user must run `go mod tidy` locally after replacing the module path placeholder. This is not a blocker but must be done before any `go build` or `go test` can succeed.

### Checklist Progress

**Step 1: Project Scaffold**
- ✅ Directory structure scaffolded (`cmd/server/`, `internal/{api,auth,chess,game,store,ws}/`, `migrations/`)
- ✅ `go.mod` initialized with all direct dependencies
- ✅ `docker-compose.yml` created (PostgreSQL 16, Redis commented out)
- ✅ `.env.example` created with all required environment variables
- ✅ `Makefile` created with all required targets
- ⬜ `make docker-up` starts PostgreSQL successfully — user must verify locally

**Documentation (pre-implementation corrections)**
- ✅ CONNECT message removed from WS protocol (CLAUDE.md)
- ✅ Time fields corrected to `whiteTimeMs`/`blackTimeMs` in ms (CLAUDE.md)
- ✅ `chess.Validator` Go interface replaced with behavioral descriptions (ARCHITECTURE.md)
- ✅ `UserStore` added to Step 3 checklist (PHASE_1.md)
- ✅ `DetectOutcome` Go signature replaced with behavioral description (PHASE_1.md)
- ✅ `github.com/prometheus/client_golang` removed from Phase 1 deps (PHASE_1.md)

### Technical Debt Introduced

None. The scaffold files contain no application logic and therefore introduce no shortcuts.

### Files Modified

- `CLAUDE.md` — CONNECT message removed, time fields corrected, session log updated
- `ARCHITECTURE.md` — chess.Validator interface replaced with behavioral descriptions
- `PHASE_1.md` — UserStore added to Step 3, DetectOutcome signature removed, prometheus dep removed
- `go.mod` — created
- `docker-compose.yml` — created
- `.env.example` — created
- `Makefile` — created
- `cmd/server/` — directory created (empty)
- `internal/api/` — directory created (empty)
- `internal/auth/` — directory created (empty)
- `internal/chess/` — directory created (empty)
- `internal/game/` — directory created (empty)
- `internal/store/` — directory created (empty)
- `internal/ws/` — directory created (empty)
- `migrations/` — directory created (empty)

### Recommended Next Step

**Step 2: Database Migrations.** Write three migration files with up/down pairs:
- `000001_create_users.up.sql` / `000001_create_users.down.sql`
- `000002_create_games.up.sql` / `000002_create_games.down.sql`
- `000003_create_moves.up.sql` / `000003_create_moves.down.sql`

Schema is fully defined in ARCHITECTURE.md. The `games` migration is the most complex — it has a `CHECK` constraint on `status`, two FK references to `users`, and a partial index on `status`. The `moves` migration has a `UNIQUE` index on `(game_id, move_number)` and a separate index on `game_id`. Verify with `make migrate-up`, `make migrate-down` (three times, one per migration), then `make migrate-up` again to confirm idempotency. Estimated time: 30–45 minutes.

---

## Part 2: Updated CLAUDE.md

```markdown
# CLAUDE.md — Session Context Document

This file is the authoritative context document for AI-assisted development sessions on this project.

**Read this first. Every session. Before writing any code.**

Update this file at the end of every session. Stale context is worse than no context.

---

## Project Identity

**Name:** chess-server
**Language:** Go 1.22+
**Type:** Learning project — production-grade chess platform, phase-by-phase
**Primary Goal:** Learn system design, distributed systems, real-time backend architecture
**NOT a goal:** Build a Chess.com competitor

---

## Current Phase

**Phase 1 — MVP**
**Status: 🔄 In Progress — Scaffold Complete, Migrations Not Started**

---

## Completed Work

### Documentation
- [x] Project purpose and scope defined
- [x] Full tech stack decided and rationale documented
- [x] All 7 documentation files created
- [x] Phase 1 spec written (PHASE_1.md)
- [x] Architecture documented (ARCHITECTURE.md)
- [x] All ADRs logged (DECISIONS_LOG.md)
- [x] Coding guidelines defined (CODING_GUIDELINES.md)

### Implementation
- [x] WebSocket infrastructure (pre-existing): connection lifecycle, read loop, write loop, heartbeats, registry, graceful shutdown — built in Go with gorilla/websocket
- [x] Step 1: Project Scaffold — go.mod, docker-compose.yml, .env.example, Makefile, directory structure

---

## Phase 1 Checklist

### Foundation
- [x] go.mod initialized with all dependencies
- [x] .env.example created
- [x] docker-compose.yml created (PostgreSQL + Redis placeholder)
- [x] Makefile created with standard targets
- [x] Directory structure scaffolded
- [ ] **ACTION REQUIRED (local):** Replace module path `github.com/yourusername/chess-server` with real GitHub path
- [ ] **ACTION REQUIRED (local):** Run `go mod tidy` to generate go.sum and resolve indirect deps
- [ ] **ACTION REQUIRED (local):** Verify `make docker-up` starts PostgreSQL successfully

### Database
- [ ] Migration 001: create users table
- [ ] Migration 002: create games table
- [ ] Migration 003: create moves table
- [ ] pgxpool connection setup (internal/store/postgres.go)
- [ ] Store interfaces defined

### Auth Layer
- [ ] JWT sign function (playerToken: gameID + userID + color)
- [ ] JWT verify function
- [ ] Anonymous userID generation

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

### Chess Layer
- [ ] notnil/chess integration
- [ ] Move validation wrapper
- [ ] Game result detection (checkmate, stalemate)
- [ ] FEN extraction after each move
- [ ] SAN move recording

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
- [ ] Store layer: unit tests with real PostgreSQL (test DB)
- [ ] Game state machine: unit tests
- [ ] Move pipeline: integration tests
- [ ] WebSocket handler: httptest-based tests
- [ ] Reconnection scenario: integration test

---

## Architectural Decisions (Summary)

Full rationale in DECISIONS_LOG.md. This is the quick-reference list.

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

---

## Technical Debt

None formally introduced yet.

When debt is added, format as:
```
TD-001: [Description] | Introduced: Phase X | Acceptable because: [reason] | Must fix by: Phase Y
```

Note: PHASE_1.md pre-declares TD-001 through TD-005 that will be introduced during implementation.

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

- **Module path placeholder:** `go.mod` uses `github.com/yourusername/chess-server`. Must be replaced with the real path before any Go files are written — every internal import path depends on this.
- **`go mod tidy` not yet run:** `go.sum` does not exist. No build or test will succeed until `go mod tidy` is run locally.
- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://` (for the migrate CLI). When migrations are wired into `main.go` at Step 13, use `pgx5://` scheme with the `golang-migrate/migrate/v4/database/pgx/v5` driver to stay consistent with pgx/v5. These are different URL schemes pointing to the same database — correct and intentional.

---

## WebSocket Message Protocol (Phase 1)

### Client → Server

```json
{ "type": "MOVE",         "san": "e4" }
{ "type": "RESIGN" }
{ "type": "PING" }
```

### Server → Client

```json
{ "type": "GAME_STATE",          "fen": "...", "turn": "white", "moves": ["e4","e5"], "status": "ACTIVE", "whiteTimeMs": 598000, "blackTimeMs": 600000 }
{ "type": "MOVE_APPLIED",        "san": "e4", "fen": "...", "turn": "black", "whiteTimeMs": 597843, "blackTimeMs": 600000 }
{ "type": "MOVE_REJECTED",       "reason": "illegal move" }
{ "type": "GAME_OVER",           "outcome": "WHITE", "reason": "CHECKMATE" }
{ "type": "OPPONENT_CONNECTED" }
{ "type": "OPPONENT_DISCONNECTED" }
{ "type": "OPPONENT_RECONNECTED" }
{ "type": "ERROR",               "code": "INVALID_TOKEN", "message": "..." }
{ "type": "PONG" }
```

---

## Key Files and Their Responsibilities

```
cmd/server/main.go           — Wires all dependencies, starts HTTP server, handles OS signals
internal/ws/connection.go    — Connection struct, read loop, write loop, heartbeat
internal/ws/registry.go      — connID -> *Connection map with RWMutex (pre-built)
internal/ws/handler.go       — HTTP upgrade, token validation, GameManager handoff
internal/game/session.go     — GameSession struct, state machine transitions
internal/game/registry.go    — gameID -> *GameSession map with RWMutex
internal/game/manager.go     — Top-level orchestrator: creates sessions, routes messages
internal/game/move.go        — Move pipeline: validate -> persist -> broadcast
internal/chess/validator.go  — Wraps notnil/chess: ValidateMove, DetectOutcome, FEN
internal/store/postgres.go   — pgxpool initialization
internal/store/game_store.go — CreateGame, GetGame, UpdateGameStatus, UpdateFEN
internal/store/move_store.go — SaveMove, GetMovesForGame
internal/store/user_store.go — CreateOrGetUser, GetUser
internal/auth/token.go       — SignPlayerToken, VerifyPlayerToken
internal/api/routes.go       — chi router, middleware stack
internal/api/game_handler.go — POST /games, POST /games/:id/join, GET /health
migrations/                  — SQL migration files (golang-migrate format)
```

---

## Next Recommended Task

**Step 2: Database Migrations**

Write three migration file pairs in `migrations/`:
- `000001_create_users.up.sql` / `000001_create_users.down.sql`
- `000002_create_games.up.sql` / `000002_create_games.down.sql`
- `000003_create_moves.up.sql` / `000003_create_moves.down.sql`

Schema is fully defined in ARCHITECTURE.md. Key things to get right:
- `games` table: `CHECK` constraint on `status`, two FK references to `users`, partial index on `status`
- `moves` table: `UNIQUE` index on `(game_id, move_number)`, separate index on `game_id`
- Every `down` migration must be a clean inverse of its `up` (drop tables/indexes in reverse dependency order)

Verify: `make migrate-up` (applies all 3), `make migrate-down` (3 times, one rollback each), `make migrate-up` again (idempotency).

---

## Session Log

| Session | Date | What Was Done |
|---------|------|---------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only — no implementation: removed CONNECT message from WS protocol (auth is URL query-param only, no post-connect auth message); fixed time fields to whiteTimeMs/blackTimeMs with millisecond values in GAME_STATE and MOVE_APPLIED samples in CLAUDE.md; replaced chess.Validator Go interface code block in ARCHITECTURE.md with behavioral descriptions; added UserStore (CreateOrGetUser, GetUser) to PHASE_1.md Step 3 checklist; replaced DetectOutcome Go signature in PHASE_1.md Step 5 with behavioral description; removed github.com/prometheus/client_golang from Phase 1 dependency list (Phase 6 only) |
| 3 | 2025-01-XX | Step 1 scaffold complete: go.mod (7 direct deps + goleak), docker-compose.yml (PostgreSQL 16 + Redis placeholder), .env.example (4 vars), Makefile (15 targets including install-tools), all 8 package directories created. go mod tidy not yet run — no go.sum exists. Module path placeholder must be replaced before coding begins. |

*Update this table at the end of every session.*
```