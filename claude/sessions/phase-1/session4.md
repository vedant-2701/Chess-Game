## Session Summary — WebSocket Port, Game Session, EventBus

### What Was Built

**`internal/ws/connection.go` — 2 targeted changes:**
- `Send()`: `websocket.BinaryMessage` → `websocket.TextMessage` (chess server sends JSON)
- `ReadLoop(registry *Registry)` → `ReadLoop(onMessage func([]byte), onClose func())` — ws layer is now fully ignorant of the game layer. Full doc comment added flagging the context lifetime decision at Step 11.

**`internal/ws/registry.go` — 3 targeted changes:**
- `"log"` → `"log/slog"`, both `log.Printf` calls replaced with `slog.Warn` using structured `"connID"` / `"error"` fields
- `registryMu` now has the mutex documentation comment per CODING_GUIDELINES

**`internal/ws/errors.go`:** Pre-existing, no changes needed.

**`internal/game/errors.go`** — New file. Three sentinel errors:
- `ErrGameNotFound` — missing in-memory session (distinct from `store.ErrGameNotFound` which is a DB miss)
- `ErrConnectionOccupied` — `RegisterConnection` called on a slot already holding a live connection
- `ErrInvalidTransition` — illegal state machine edge attempted

**`internal/game/session.go`** — New file. Full `GameSession` implementation:
- Struct with `sync.RWMutex` protecting 10 fields (documented per CODING_GUIDELINES)
- `NewGameSession(id, whiteID string) *GameSession` — initialises board via `internalchess.NewGame()`, both clocks to `InitialTimeMs` (600,000 ms), status to `WAITING_FOR_PLAYER`
- `SetPlayerBlack`, `RegisterConnection` (errors on occupied slot), `ReplaceConnection` (unconditional overwrite for reconnection), `ClearConnection`
- `BothPlayersConnected() bool` — read-lock snapshot
- `Transition(newStatus store.GameStatus) error` — driven by `validTransitions` map, returns `ErrInvalidTransition` for all illegal edges; COMPLETED and ABANDONED are terminal
- `SetOutcome(outcome, reason)` — called after `Transition(COMPLETED)` by the move pipeline
- `UpdateClocks(whiteMs, blackMs int64)` — called by Clock (Step 9)
- `CurrentStateSnapshot() GameStateSnapshot` — full consistent read under RLock; derives turn via `boardTurn()` helper mapping `notnil.Color` → `store.Color`
- `SendToPlayer(color, msg) error` — snapshots connection pointer under RLock, sends outside lock
- `SendToBothPlayers(msg)` — same pattern; per-player failures logged at Warn, never abort the other send

**`internal/game/registry.go`** — New file. `GameRegistry` with `sync.RWMutex`:
- `Register`, `Get` (returns `ErrGameNotFound`), `Unregister` (no-op if missing), `AllActive` (snapshot-then-release, always non-nil slice)

**`internal/game/messages.go`** — New file. Single source of truth for all WebSocket protocol strings:
- Client→Server message types: `MsgTypeMove`, `MsgTypeResign`, `MsgTypePing`
- Server→Client message types: `MsgTypePong`, `MsgTypeGameState`, `MsgTypeMoveApplied`, `MsgTypeMoveRejected`, `MsgTypeGameOver`, `MsgTypeOpponentConnected`, `MsgTypeOpponentDisconnected`, `MsgTypeOpponentReconnected`, `MsgTypeError`
- Rejection reasons: `RejectReasonNotYourTurn`, `RejectReasonIllegalMove`, `RejectReasonGameNotActive`
- Error codes: `ErrCodeInvalidToken`, `ErrCodeGameNotFound`, `ErrCodeGameFull`, `ErrCodeInternalError`

**`internal/game/eventbus.go`** — New file. `EventBus` interface + `LocalEventBus` implementation:
- `GameEvent{GameID, Type, Payload}` — Type reuses `MsgType*` constants; Payload is pre-serialised JSON forwarded directly to `ws.Connection.Send`
- `EventBus` interface: `Publish(ctx, GameEvent) error` + `Subscribe(ctx, gameID) (<-chan GameEvent, func(), error)`
- `LocalEventBus`: `subscribers map[string]map[chan GameEvent]struct{}`; buffer size 8 per subscriber
- `Publish` holds `mu.RLock()` for the entire non-blocking send loop — the only synchronisation that prevents a race between Publish and channel close on unsubscribe. Drops events to full channels with `slog.Warn`, never blocks.
- `unsubscribe` closes the channel under `mu.Lock()` — safe because Publish cannot be mid-send while the write lock is held
- Compile-time interface check: `var _ EventBus = (*LocalEventBus)(nil)`

**Tests — all passing with `-race`:**
- `internal/game/session_test.go`: 10 tests covering all valid transitions, all invalid transitions (7 illegal edges), `RegisterConnection` success/occupied, `ReplaceConnection`, `ClearConnection`, `BothPlayersConnected` lifecycle, `CurrentStateSnapshot` initial state and `SetPlayerBlack` reflection
- `internal/game/registry_test.go`: `Register`+`Get`, `Get` missing → `ErrGameNotFound`, `Unregister`, `Unregister` missing is no-op, `AllActive` empty→non-nil, `AllActive` count, concurrent `Register`+`Get`+`Unregister` under `-race`
- `internal/game/eventbus_test.go`: no-subscriber publish, single subscriber receives event, unsubscribe stops delivery, two subscribers same game both receive, cross-game isolation, full channel drops without panic or block

### Decisions Made

**`SendToPlayer` and `SendToBothPlayers` added to `GameSession` (not in PHASE_1.md checklist).** The move pipeline (Step 8) has no other way to push messages to players without exposing `*ws.Connection` outside the session. Adding these methods is the correct encapsulation — `*ws.Connection` stays fully contained inside `GameSession`. Not a new ADR; follows directly from the two-registry architecture in ADR-009.

**`UpdateClocks` added to `GameSession` (not in PHASE_1.md checklist).** The Clock (Step 9) needs to persist remaining time back to the session before the store write. Added now to avoid an awkward retrofit at Step 9.

**`messages.go` as the single source of truth for WebSocket protocol strings.** All magic strings (message types, rejection reasons, error codes) live in `internal/game/messages.go`. The `ws` layer is byte-transparent and needs none of these. Error codes used by `ws.Handler` at Step 11 (e.g. `ErrCodeInvalidToken`) are also defined here — the handler will import `internal/game` for them. No separate `internal/proto` package: there is no overlap between what `ws` and `game` need from the protocol, so a shared package would be indirection with no payoff.

**`Publish` holds `mu.RLock()` for the full send loop.** This is the critical correctness decision for `LocalEventBus`. The alternative — snapshot the subscriber map under RLock, release, then send — creates a window where a subscriber can be deleted and its channel closed between the snapshot and the send, causing a send-on-closed-channel panic. Holding RLock during the non-blocking sends prevents this at zero cost (sends are O(1) channel ops). No ADR needed; documented in code comment.

**"Player-to-connection bridge for reconnection" checklist item is already complete.** Confirmed this session: the bridge is `GameSession.playerWhite`/`playerBlack` (the pointer slots) + `ReplaceConnection()` + `CurrentStateSnapshot()`. The Manager (Step 10) is the caller, not a new data structure. Item closed.

### Tradeoffs Considered

**Shared `internal/proto` package for WebSocket protocol strings vs. `internal/game/messages.go`.** A shared package was initially raised by Vedant. On analysis: `internal/ws` is byte-transparent — it never inspects message content. Only `internal/game` constructs and parses messages. A shared package would add a new node to the dependency graph with no consumer outside `internal/game`. Rejected. `messages.go` is the right home.

**`Publish` snapshot-then-send vs. Publish holds RLock.** Snapshot-then-send looks cleaner but has a correctness bug: unsubscribe can close a channel between snapshot and send. Holding RLock is the correct solution. The cost is that unsubscribers block during in-flight publishes — but publishes are O(1) non-blocking channel selects, so the block duration is negligible.

**`ReadLoop` accepting a `ws.Sender` interface vs. accepting `func([]byte)` callback.** An interface would be cleaner if there were multiple implementations. There is exactly one implementation (`game.Manager.HandleMessage`). A function callback is simpler and sufficient. If a second consumer ever appears, the callback can be wrapped in an adapter without changing `ReadLoop`'s signature.

### Lessons Learned

**`notnil/chess` package-level vs. method API split matters at the call site.** `NewGame()`, `CurrentFEN()`, `MoveHistory()` are package-level functions. `ValidateMove`, `ApplyMove`, `DetectOutcome` are methods on `*Validator`. `GameSession` only needs the package-level functions — no `*Validator` field on the session struct. The `*Validator` belongs in the move pipeline (Step 8), not in the session.

### Problems Encountered

No problems encountered

### Checklist Progress

**Step 6: Game Session and Registry**
- ✅ `GameSession` struct with documented mutex scope
- ✅ `NewGameSession(id, whiteID string) *GameSession`
- ✅ `SetPlayerBlack(userID string)`
- ✅ `RegisterConnection(color, conn) error`
- ✅ `ReplaceConnection(color, conn)` — reconnection path
- ✅ `ClearConnection(color)` — disconnect path
- ✅ `BothPlayersConnected() bool`
- ✅ `Transition(newStatus) error` — all valid and invalid edges tested
- ✅ `CurrentStateSnapshot() GameStateSnapshot`
- ✅ `SendToPlayer` and `SendToBothPlayers` — move pipeline prerequisites
- ✅ `UpdateClocks` — clock layer prerequisite
- ✅ `GameRegistry` with `Register`, `Get`, `Unregister`, `AllActive`
- ✅ Unit tests — all passing with `-race`
- ✅ Player-to-connection bridge for reconnection — confirmed complete (uses session pointer slots + `ReplaceConnection`)

**Step 7: EventBus**
- ✅ `GameEvent{GameID, Type, Payload}`
- ✅ `EventBus` interface: `Publish`, `Subscribe`
- ✅ `LocalEventBus` implementation
- ✅ `NewLocalEventBus()`
- ✅ `messages.go` — single source of truth for all WebSocket protocol strings
- ✅ Unit tests — publish/subscribe, unsubscribe, multi-subscriber, cross-game isolation, full channel drop — all passing with `-race`

### Technical Debt Introduced

None this session.

### Files Modified

**Modified:**
- `internal/ws/connection.go` — `ReadLoop` signature, `TextMessage`
- `internal/ws/registry.go` — `slog`, mutex comment

**Created:**
- `internal/game/errors.go`
- `internal/game/session.go`
- `internal/game/registry.go`
- `internal/game/messages.go`
- `internal/game/eventbus.go`
- `internal/game/session_test.go`
- `internal/game/registry_test.go`
- `internal/game/eventbus_test.go`

### Recommended Next Step

**Step 8: Move Pipeline — implement `internal/game/move.go`.**

`MoveProcessor` struct depending on `chess.Validator`, `store.GameStore`, `store.MoveStore`, `EventBus`. Single public method: `ProcessMove(ctx, session *GameSession, color store.Color, san string) error`. Pipeline sequence: turn check → `ValidateMove` → `store.MoveStore.SaveMove` → `store.GameStore.UpdateCurrentFEN` → `ApplyMove` → clock switch via `session.UpdateClocks` → `DetectOutcome` → publish `MOVE_APPLIED` or `GAME_OVER` via EventBus. Integration tests required (real PostgreSQL, `//go:build integration` tag): full pipeline happy path, wrong turn rejection, illegal move rejection, DB failure leaves board unchanged, checkmate detected and `GAME_OVER` published.

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
**Status: 🔄 In Progress — Steps 1–7 Complete, Step 8 (Move Pipeline) Next**

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

### Game Layer
- [x] GameSession struct defined
- [x] GameState machine (WAITING → ACTIVE → COMPLETED / ABANDONED)
- [x] GameRegistry (gameID → *GameSession)
- [x] EventBus interface defined (LocalEventBus for Phase 1)
- [x] Player-to-connection bridge for reconnection (session pointer slots + ReplaceConnection)
- [x] messages.go — single source of truth for all WebSocket protocol strings

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
- [x] Game session and registry: unit tests (no build tag, no database)
- [x] EventBus: unit tests (no build tag, no database)
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

- **`MoveHistory()` returns annotated SAN.** `AlgebraicNotation.Encode` appends `+` for check and `#` for checkmate. The `"moves"` field in `GAME_STATE` messages will contain annotated SAN. Step 8 move pipeline must source move history from `chess.MoveHistory(session.board)`, not from a cached copy of raw client input.

- **`DetectOutcome` must be called after `ApplyMove`.** Calling it before any move is played will always return `(nil, false)` even for checkmate/stalemate positions loaded from FEN.

- **`ReadLoop` context decision deferred to Step 11.** The HTTP request context (`r.Context()`) is cancelled when `ServeHTTP` returns, which is before `ReadLoop` exits. At Step 11, decide whether to pass `r.Context()`, a derived context, or `context.Background()` to `HandleMessage`. The comment in `ReadLoop` flags this explicitly.

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** This is intentional. Do not "optimise" it to snapshot-then-release — that pattern creates a window where `unsubscribe` can close a channel between the snapshot and the send, causing a send-on-closed-channel panic.

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
internal/game/errors.go       — ErrGameNotFound, ErrConnectionOccupied, ErrInvalidTransition
internal/game/messages.go     — All WebSocket message type strings, rejection reasons, error codes
internal/game/session.go      — GameSession struct, state machine, connection management, snapshots
internal/game/registry.go     — gameID -> *GameSession map with RWMutex
internal/game/eventbus.go     — EventBus interface + LocalEventBus implementation
internal/game/manager.go      — Top-level orchestrator: creates sessions, routes messages [Step 10]
internal/game/move.go         — Move pipeline: validate -> persist -> broadcast [Step 8]
internal/game/clock.go        — Per-game clock, timeout detection goroutine [Step 9]
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
internal/api/routes.go        — chi router, middleware stack [Step 12]
internal/api/game_handler.go  — POST /games, POST /games/:id/join, GET /games/:id, GET /health [Step 12]
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
    slog.Error("ApplyMove failed after ValidateMove succeeded", "gameID", gameID, "san", san, "error", err)
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

Methods on *Validator (used in move pipeline, Step 8):
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

**Step 8: Move Pipeline — implement `internal/game/move.go`.**

`MoveProcessor` struct with dependencies: `*chess.Validator`, `store.GameStore`, `store.MoveStore`, `EventBus`. Single public method: `ProcessMove(ctx context.Context, session *GameSession, color store.Color, san string) error`.

Pipeline sequence (must be in this exact order per ADR-013 and persistence-first invariant):
1. Validate it is `color`'s turn (check `session.CurrentStateSnapshot().Turn`)
2. Validate game is ACTIVE (check `session.CurrentStateSnapshot().Status`)
3. `validator.ValidateMove(session.board, san)` — no mutation
4. `moveStore.SaveMove(ctx, move)` — DB write first
5. `gameStore.UpdateCurrentFEN(ctx, gameID, fenAfter)` — DB update
6. `validator.ApplyMove(session.board, san)` — mutate only after DB confirms
7. Clock switch via `session.UpdateClocks(whiteMs, blackMs)` + `gameStore.UpdateClocks(ctx, ...)`
8. `validator.DetectOutcome(session.board)` — check for game end
9a. If outcome: `session.Transition(COMPLETED)`, `session.SetOutcome(...)`, update DB, publish `GAME_OVER` event
9b. If no outcome: publish `MOVE_APPLIED` event

Integration tests required (`//go:build integration`, real PostgreSQL): full pipeline happy path, wrong turn rejected with `MOVE_REJECTED`, illegal move rejected with board unchanged, DB failure on SaveMove leaves board unchanged, Scholar's mate triggers `GAME_OVER` with CHECKMATE reason.

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
```