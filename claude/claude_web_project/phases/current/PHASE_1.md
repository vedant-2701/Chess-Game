# Phase 1 — MVP Specification

This document is the authoritative specification for Phase 1. It defines scope, the implementation checklist, the full API and WebSocket protocol contracts, and acceptance criteria. Phase 1 is not complete until every acceptance criterion is verified, not just every checklist item checked.

**Status: 🔄 In Progress**

---

## Objective

Build a functional two-player chess server where two players can play a complete game of chess in real time via a shared game link, with full state recovery on disconnect or server restart.

**The single most important learning outcome of Phase 1:**
Understanding what "server-authoritative state" means in practice, and building reconnection that actually works on real networks — not just on localhost.

---

## Scope

### In Scope

- Two players play chess via a shared game link (no matchmaking)
- Anonymous player identity (UUID generated client-side, no account required)
- JWT player tokens scoped per game (authenticate WebSocket connections, enable reconnection)
- Legal move validation server-side (using notnil/chess)
- Game state persisted to PostgreSQL after every move (before broadcast)
- Reconnection: player presents token, receives full current game state
- Server restart recovery: active games resume from database state
- Game result detection: checkmate, stalemate, resignation
- Time controls: 10+0 (ten minutes per player, no increment), server-side clock only
- Timeout: player whose clock reaches zero loses, detected server-side
- Single time control only — no choice of time format in Phase 1

### Explicitly Out of Scope

Do not build these. Do not design for them. Do not add "just in case" hooks for them.

| Feature | Why Out of Scope |
|---------|-----------------|
| Matchmaking queue | Phase 3 learning objective |
| ELO / ratings | Phase 4 learning objective |
| Game history browsing | Phase 4 learning objective |
| Spectators | Phase 5 learning objective |
| Chat | Not a system design concept for this project |
| Bots | AI problem, not systems problem |
| Multiple time controls | Unnecessary complexity for Phase 1 |
| Draw offers | Stalemate auto-detected; manual draw is Phase 4 |
| Account registration/login | Post-Phase 1 |
| Redis | Phase 2 — EventBus interface handles this seam |
| Horizontal scaling | Phase 2 learning objective |
| PGN export | Phase 4 |
| Analysis board | Post-Phase 5 |
| Tournaments | Post-Phase 5 |

---

## API Contract

### REST Endpoints

#### `POST /games`

Creates a new game. Returns gameID and White's player token.

**Request:**
```json
{
  "userID": "uuid-generated-client-side"
}
```

**Response 201:**
```json
{
  "data": {
    "gameID": "550e8400-e29b-41d4-a716-446655440000",
    "playerToken": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "color": "WHITE",
    "joinURL": "/game/550e8400-e29b-41d4-a716-446655440000"
  }
}
```

**What happens internally:**
1. Generate gameID (UUID v4)
2. Create user record if userID not found (or create anonymous user)
3. Insert game record with status `WAITING_FOR_PLAYER`, player_white_id set
4. Sign JWT: `{ gameID, userID, color: "WHITE", exp: now+24h }`
5. Return

**Error responses:**
- `400` — missing or invalid userID format
- `500` — database failure

---

#### `POST /games/:id/join`

Second player joins an existing game. Returns Black's player token.

**Request:**
```json
{
  "userID": "uuid-generated-client-side"
}
```

**Response 200:**
```json
{
  "data": {
    "gameID": "550e8400-e29b-41d4-a716-446655440000",
    "playerToken": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
    "color": "BLACK"
  }
}
```

**What happens internally:**
1. Fetch game by ID
2. Verify game status is `WAITING_FOR_PLAYER`
3. Verify the joining userID is not the same as player_white_id (cannot play yourself)
4. Update game: set player_black_id, status → `WAITING_FOR_PLAYER` (stays until both connect via WS)
5. Sign JWT: `{ gameID, userID, color: "BLACK", exp: now+24h }`
6. Return

**Error responses:**
- `404` — game not found
- `409` — game already has two players (status is not WAITING_FOR_PLAYER)
- `409` — userID matches player_white_id (cannot play yourself)
- `400` — missing or invalid request

---

#### `GET /games/:id`

Returns current game state. Used by client on page load to determine if game exists before opening WebSocket.

**Response 200:**
```json
{
  "data": {
    "gameID": "550e8400-e29b-41d4-a716-446655440000",
    "status": "ACTIVE",
    "currentFEN": "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
    "outcome": null,
    "outcomeReason": null
  }
}
```

**Error responses:**
- `404` — game not found

---

#### `GET /health`

Liveness check.

**Response 200:**
```json
{ "status": "ok" }
```

---

### WebSocket Endpoint

#### `GET /ws/game/:id?token=<playerToken>`

Upgrades to WebSocket. The player token is passed as a query parameter because browser WebSocket API does not support custom headers.

**Connection flow:**
1. Server validates token signature and expiry
2. Server extracts `{ gameID, userID, color }` from token
3. Server verifies gameID in token matches `:id` in URL path
4. If valid: upgrade to WebSocket, register connection into GameSession
5. If invalid: HTTP 401 before upgrade, close connection

**Note on token in query parameter:** This means the token appears in server access logs. Acceptable for Phase 1 given no sensitive PII is in the token. In production this would be mitigated by sending the token in the first WebSocket message after connection instead.

---

## WebSocket Message Protocol

All messages are JSON. Both client and server send JSON objects with a mandatory `type` field.

### Client → Server Messages

#### MOVE
```json
{
  "type": "MOVE",
  "san": "e4"
}
```
Submits a move in Standard Algebraic Notation. The server validates it is the correct player's turn and that the move is legal.

#### RESIGN
```json
{
  "type": "RESIGN"
}
```
Player concedes the game. Opponent wins immediately.

#### PING
```json
{
  "type": "PING"
}
```
Application-level ping (separate from WebSocket protocol-level ping/pong). Server responds with PONG. Used by client to verify the connection is alive.

---

### Server → Client Messages

#### GAME_STATE
Sent immediately after a player connects or reconnects. Full current game state.
```json
{
  "type": "GAME_STATE",
  "fen": "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1",
  "turn": "WHITE",
  "moves": ["e4", "e5", "Nf3"],
  "status": "ACTIVE",
  "whiteTimeMs": 598000,
  "blackTimeMs": 600000,
  "outcome": null,
  "outcomeReason": null
}
```

#### MOVE_APPLIED
Sent to both players after a valid move is persisted and applied.
```json
{
  "type": "MOVE_APPLIED",
  "san": "e4",
  "fen": "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR b KQkq e3 0 1",
  "turn": "BLACK",
  "moveNumber": 1,
  "whiteTimeMs": 597843,
  "blackTimeMs": 600000
}
```

#### MOVE_REJECTED
Sent only to the player who submitted the illegal move.
```json
{
  "type": "MOVE_REJECTED",
  "san": "e5",
  "reason": "not your turn"
}
```
Possible reasons: `"not your turn"`, `"illegal move"`, `"game not active"`

#### GAME_OVER
Sent to both players when the game ends for any reason.
```json
{
  "type": "GAME_OVER",
  "outcome": "WHITE",
  "reason": "CHECKMATE",
  "fen": "final board FEN"
}
```
`outcome`: `"WHITE"` | `"BLACK"` | `"DRAW"`
`reason`: `"CHECKMATE"` | `"STALEMATE"` | `"RESIGNATION"` | `"TIMEOUT"` | `"ABANDONED"`

**Note on `ABANDONED` outcome pairing (corrected post-Step-10, see DECISIONS_LOG_PHASE_1.md):** `reason: "ABANDONED"` can pair with EITHER a winner (`"WHITE"`/`"BLACK"`) or `"DRAW"`, depending on whether one or both players were disconnected when the 60-second window elapsed. See the corrected Game State Machine section below — single-player abandonment is a COMPLETED game with a winner, not a drawn ABANDONED game.

#### OPPONENT_CONNECTED
Sent when the opponent connects for the first time (game transitions to ACTIVE).
```json
{
  "type": "OPPONENT_CONNECTED"
}
```

#### OPPONENT_DISCONNECTED
Sent when the opponent's WebSocket connection drops.
```json
{
  "type": "OPPONENT_DISCONNECTED"
}
```

#### OPPONENT_RECONNECTED
Sent when a previously disconnected opponent reconnects.
```json
{
  "type": "OPPONENT_RECONNECTED"
}
```

#### ERROR
Sent when the server encounters a client-attributable error that is not a move rejection.
```json
{
  "type": "ERROR",
  "code": "INVALID_TOKEN",
  "message": "player token is expired"
}
```
Error codes: `"INVALID_TOKEN"`, `"GAME_NOT_FOUND"`, `"GAME_FULL"`, `"INTERNAL_ERROR"`

#### PONG
Response to client PING.
```json
{
  "type": "PONG"
}
```

---

## Game State Machine

**CORRECTED (post-Step-10, see DECISIONS_LOG_PHASE_1.md ADR-015):** The diagram and rule below originally described only the both-players-disconnected case. As written, a single player who disconnected and never reconnected left the game stuck ACTIVE forever — the actual intended (and now implemented) behavior also handles single-player disconnection.

```
[POST /games]
     │
     ▼
WAITING_FOR_PLAYER
     │
     │ Black player connects via WebSocket
     │ (both players now have WS connections)
     ▼
   ACTIVE ◄────────────────────────────────────────┐
     │                                              │
     │                                         Player disconnects,
     │                                         reconnects within
     ├── Checkmate detected                    60-second window
     ├── Stalemate detected                         │
     ├── Timeout (clock reaches zero)               └────────────────
     ├── Player resigns
     │
     ▼
  COMPLETED
     │
     └── (terminal state — no transitions out)

ACTIVE ──► COMPLETED  (winner = the player who stayed connected)
     │
     └── One player disconnects and does NOT reconnect within 60
         seconds, WHILE the opponent remains connected the entire time.
         outcome = opponent's color, outcome_reason = ABANDONED.

ACTIVE ──► ABANDONED  (no winner — draw)
     │
     └── BOTH players are disconnected at the moment the 60-second
         timer (started by whichever of them disconnected first) fires,
         and neither has reconnected.
         outcome = DRAW, outcome_reason = ABANDONED.
```

**Abandonment rule (corrected):** When a player disconnects, a 60-second timer starts for that player. If they reconnect before the timer fires, the timer cancels and the game resumes with no state change.

When the timer fires, the outcome depends on whether the *opponent* is connected at that moment:
- **Opponent still connected:** the disconnected player loses by abandonment. The game transitions to `COMPLETED` with the connected player as winner and `outcome_reason: ABANDONED`. This is the common case — one player's connection drops while the other is actively present and waiting.
- **Opponent also disconnected:** the game transitions to `ABANDONED` (terminal, drawn — `outcome: DRAW`, `outcome_reason: ABANDONED`).

`ABANDONED` status is reserved exclusively for the both-disconnected, no-winner case. A single-player abandonment is recorded as `COMPLETED` with a winner, even though `outcome_reason` is still `ABANDONED` in both cases — the `status` field, not the `outcome_reason` field, is what distinguishes them.

**Clock behavior on disconnect:** When a player disconnects, their clock is paused. It resumes when they reconnect. This is a Phase 1 simplification — in production (Phase 4+), the clock would continue running to prevent disconnect-as-stall-tactic.

---

## Implementation Checklist

Items must be completed in order within each section. Do not skip ahead.

### Step 1: Project Scaffold
- [x] `go mod init github.com/<username>/chess-server`
- [x] Add all dependencies to go.mod and run `go mod tidy`
- [x] Create directory structure as defined in ARCHITECTURE.md
- [x] Create `docker-compose.yml` (PostgreSQL 16, Redis placeholder commented out)
- [x] Create `.env.example` with all required environment variables
- [x] Create `Makefile` with: run, build, test, test-race, migrate-up, migrate-down, docker-up, docker-down, lint
- [x] Verify `make docker-up` starts PostgreSQL successfully

**Dependencies to add:**
```
github.com/gorilla/websocket v1.5.x
github.com/go-chi/chi/v5 v5.x.x
github.com/notnil/chess v1.x.x
github.com/jackc/pgx/v5 v5.x.x
github.com/golang-migrate/migrate/v4 v4.x.x
github.com/golang-jwt/jwt/v5 v5.x.x
github.com/stretchr/testify v1.x.x
```

---

### Step 2: Database Migrations
- [x] Migration 001 up/down: `users` table
- [x] Migration 002 up/down: `games` table with all columns and constraints
- [x] Migration 003 up/down: `moves` table with indexes
- [x] Verify `make migrate-up` applies all three migrations cleanly
- [x] Verify `make migrate-down` rolls back one migration at a time
- [x] Verify `make migrate-up` is idempotent (run twice, no error)

Schema is defined in ARCHITECTURE.md. Do not deviate from it without updating ARCHITECTURE.md.

---

### Step 3: Store Layer
- [x] `internal/store/postgres.go`: `NewPool(ctx, databaseURL)` returning `*pgxpool.Pool`
- [x] `internal/store/game_store.go`:
  - [x] `CreateGame(ctx, game *Game) error`
  - [x] `GetGame(ctx, id string) (*Game, error)` — returns `ErrGameNotFound` if not found
  - [x] `UpdateGameStatus(ctx, id string, status GameStatus, outcome *Outcome) error`
  - [x] `UpdateCurrentFEN(ctx, id string, fen string) error`
  - [x] `UpdatePlayerBlack(ctx, id string, playerBlackID string) error`
  - [x] `GetActiveGames(ctx) ([]*Game, error)` — for server restart recovery
  - [x] `UpdateClocks(ctx, id string, whiteMs, blackMs int64) error`
- [x] `internal/store/move_store.go`:
  - [x] `SaveMove(ctx, move *Move) error`
  - [x] `GetMovesForGame(ctx, gameID string) ([]*Move, error)`
- [x] `internal/store/user_store.go`:
  - [x] `CreateOrGetUser(ctx, userID string) (*User, error)` — upsert: inserts the user record if the userID does not exist, returns the existing record if it does
  - [x] `GetUser(ctx, id string) (*User, error)` — returns `ErrUserNotFound` if not found
- [x] Unit tests for all store methods (integration tag, real PostgreSQL)
- [x] Verify error wrapping: all errors include function name and relevant IDs

---

### Step 4: Auth Layer
- [x] `internal/auth/token.go`:
  - [x] `PlayerClaims` struct: `{ GameID, UserID, Color, RegisteredClaims }`
  - [x] `SignPlayerToken(claims PlayerClaims, secret string) (string, error)`
  - [x] `VerifyPlayerToken(token string, secret string) (*PlayerClaims, error)`
- [x] Unit tests:
  - [x] Valid token signs and verifies correctly
  - [x] Expired token returns error
  - [x] Tampered token returns error
  - [x] Wrong secret returns error

---

### Step 5: Chess Layer
- [x] `internal/chess/validator.go`:
  - [x] `NewValidator() *Validator`
  - [x] `NewGame() *chess.Game` — returns starting position
  - [x] `GameFromFEN(fen string) (*chess.Game, error)`
  - [x] `GameFromMoves(moves []string) (*chess.Game, error)` — replay move history
  - [x] `ValidateAndApply(game *chess.Game, san string) (*chess.Game, error)`
  - [x] DetectOutcome: given a game state, return whether the game is over, who won (or draw), and the reason (checkmate/stalemate). Exact signature decided at implementation time.
  - [x] `CurrentFEN(game *chess.Game) string`
  - [x] `MoveHistory(game *chess.Game) []string`
- [x] Unit tests:
  - [x] Valid moves apply correctly
  - [x] Illegal moves return error (wrong turn, illegal piece movement)
  - [x] Checkmate detected on Scholar's mate position
  - [x] Stalemate detected
  - [x] En passant validates correctly
  - [x] Castling validates correctly
  - [x] FEN round-trips (GameFromFEN then CurrentFEN returns same FEN)

---

### Step 6: Game Session and Registry
- [x] `internal/game/session.go`:
  - [x] `GameSession` struct (see ARCHITECTURE.md for fields)
  - [x] `NewGameSession(id string, whiteID string) *GameSession`
  - [x] `SetPlayerBlack(userID string)`
  - [x] `RegisterConnection(color Color, conn *ws.Connection) error`
  - [x] `ReplaceConnection(color Color, conn *ws.Connection)` — for reconnection
  - [x] `ClearConnection(color Color)` — on disconnect
  - [x] `BothPlayersConnected() bool`
  - [x] `Transition(newState GameState) error` — validates legal transitions
  - [x] `CurrentStateSnapshot() GameStateSnapshot` — thread-safe read of full state
- [x] `internal/game/registry.go`:
  - [x] `GameRegistry` struct
  - [x] `NewGameRegistry() *GameRegistry`
  - [x] `Register(session *GameSession)`
  - [x] `Get(gameID string) (*GameSession, error)`
  - [x] `Unregister(gameID string)`
  - [x] `AllActive() []*GameSession` — for server restart / clock recovery
- [x] Unit tests for state machine transitions (all valid and invalid transitions)

---

### Step 7: EventBus
- [x] `internal/game/eventbus.go`:
  - [x] `GameEvent` struct: `{ GameID string, Type string, Payload []byte }`
  - [x] `EventBus` interface: `Publish`, `Subscribe`
  - [x] `LocalEventBus` implementation (in-process, for Phase 1)
  - [x] `NewLocalEventBus() *LocalEventBus`
- [x] Unit tests: publish then subscribe receives event, unsubscribe stops delivery

---

### Step 8: Move Pipeline
- [x] `internal/game/move.go`:
  - [x] `MoveProcessor` struct (depends on: chess.Validator, store.GameStore, store.MoveStore, EventBus)
  - [x] `ProcessMove(ctx context.Context, session *GameSession, color Color, san string) error`
    - Validates it is the correct player's turn
    - Validates move legality
    - Persists move to database
    - Updates current_fen in database
    - Publishes MOVE_APPLIED event via EventBus
    - Checks for game outcome
    - If outcome: updates game status in DB, publishes GAME_OVER event
- [x] Integration tests:
  - [x] Full move pipeline: receive → persist → broadcast
  - [x] Wrong turn: rejected with correct error
  - [x] Illegal move: rejected, board state unchanged
  - [x] Database failure during persist: move rejected, board state unchanged
  - [x] Checkmate detected and GAME_OVER published

---

### Step 9: Clock
- [x] `internal/game/clock.go`:
  - [x] `Clock` struct: per-player timers, active color tracking
  - [x] `NewClock(initialMs int64) *Clock`
  - [x] `Start(color Color)` — begins counting down for that color
  - [x] `Switch()` — stops active color's clock, starts opponent's
  - [x] `Pause()` — stops active clock without switching (on disconnect)
  - [x] `Resume(color Color)` — resumes paused clock for given color
  - [x] `TimeRemaining(color Color) time.Duration`
  - [x] `SetTimeoutCallback(fn func(Color))` — called when a clock hits zero
  - [x] Clock goroutine runs independently, fires callback on timeout
  - [x] `Stop()` — terminates clock goroutine cleanly
- [x] Unit tests:
  - [x] Clock counts down correctly
  - [x] Switch updates active player
  - [x] Timeout callback fires at correct time
  - [x] Pause/resume preserves remaining time
  - [x] Stop terminates goroutine (no goroutine leak — verify with goleak or manual check)

---

### Step 10: Game Manager
- [x] `internal/game/manager.go`:
  - [x] `Manager` struct (depends on: GameRegistry, MoveProcessor, store.GameStore, store.MoveStore, auth.TokenService, EventBus)
  - [x] `NewManager(...) *Manager`
  - [x] `CreateGame(ctx, userID string) (*GameSession, string, error)` — returns session + playerToken
  - [x] `JoinGame(ctx, gameID, userID string) (string, error)` — returns playerToken
  - [x] `HandleConnect(ctx, gameID string, color Color, conn *ws.Connection) error`
  - [x] `HandleDisconnect(gameID string, color Color)`
  - [x] `HandleMessage(ctx, gameID string, color Color, raw []byte) error`
  - [x] `RestoreActiveGames(ctx) error` — called on server startup, hydrates GameRegistry from DB
- [x] Message routing in HandleMessage:
  - [x] MOVE → MoveProcessor.ProcessMove
  - [x] RESIGN → update game status, broadcast GAME_OVER
  - [x] PING → send PONG to sender only
  - [x] Unknown type → send ERROR to sender

---

### Step 11: WebSocket Handler

**Location correction (2026-07-02, see ARCHITECTURE.md “internal/ws” and “internal/api” sections):** This step was originally specified as `internal/ws/handler.go` with a `Handler` holding a `*game.Manager` field. That is a circular import: `internal/game` already imports `internal/ws` for `*ws.Connection`, so `internal/ws` cannot import `internal/game` back. The handler lives in `internal/api` instead, as `WSHandler`, which is the layer already documented to depend on `internal/game`.

- [x] `internal/api/ws_handler.go`:
  - [x] `WSHandler` struct (depends on: token-verify function/`*auth` package, `*game.Manager`, `*ws.Registry`, a server-lifetime `context.Context` per ADR-018)
  - [x] `ServeHTTP(w http.ResponseWriter, r *http.Request)` — upgrades and handles lifecycle
  - [x] Extract token from query parameter `?token=`
  - [x] Verify token, extract claims
  - [x] Verify claims.GameID matches URL parameter :id
  - [x] Register connection into ws.Registry
  - [x] Call game.Manager.HandleConnect
  - [x] Read loop: deserialize messages, route to game.Manager.HandleMessage using the server-lifetime context (ADR-018), not `r.Context()`
  - [x] On disconnect: call game.Manager.HandleDisconnect, unregister from ws.Registry
- [x] Integration tests (in `internal/api/ws_handler_test.go`):
  - [x] Invalid token: connection refused with appropriate close code
  - [x] Valid token: connection accepted, GAME_STATE received
  - [x] Reconnection: second connection with same token receives current GAME_STATE

---

### Step 12: HTTP API Handlers
- [x] `internal/api/game_handler.go`:
  - [x] `GameHandler` struct (depends on: game.Manager, *and* `*store.UserStore` — deviation from the literal spec, see CLAUDE.md Implementation Decisions: Manager.CreateGame/JoinGame assume the user row already exists, and user upsert is an HTTP-layer concern)
  - [x] `CreateGame(w, r)` — POST /games
  - [x] `JoinGame(w, r)` — POST /games/:id/join
  - [x] `GetGame(w, r)` — GET /games/:id
  - [x] `Health(w, r)` — GET /health (response deliberately NOT wrapped in the `{"data":...}` envelope — see CLAUDE.md Implementation Decisions)
- [x] `internal/api/routes.go`:
  - [x] chi router setup
  - [x] Request logging middleware (slog)
  - [x] Panic recovery middleware (chi's `middleware.Recoverer` — third-party, not held to §4's slog-only rule, see CLAUDE.md)
  - [x] Route registration
- [x] Unit tests for all handlers (integration-tagged, real PostgreSQL via shared testPool — no mocks, per CODING_GUIDELINES.md §6; PHASE_1.md's original "no real DB in handler tests" note was superseded once GameHandler took on a direct store dependency)

---

### Step 13: Main and Wiring
- [x] `cmd/server/main.go`:
  - [x] Load config from environment (DATABASE_URL, JWT_SECRET, SERVER_PORT, LOG_LEVEL)
  - [x] Initialize pgxpool
  - [x] Run pending migrations on startup
  - [x] Initialize all layers in dependency order
  - [x] Call `manager.RestoreActiveGames(ctx)` — hydrate in-memory state from DB
  - [x] Register routes
  - [x] Start HTTP server
  - [x] Listen for OS signals (SIGTERM, SIGINT)
  - [x] On signal: stop accepting new games, wait for in-progress moves to complete, persist clock state, graceful shutdown of WebSocket registry, close DB pool
- [x] Verify server starts and `GET /health` returns 200

---

### Step 14: End-to-End Verification

These are manual tests run against the running server before Phase 1 is declared complete.

- [x] Two browser tabs complete a full game from first move to checkmate
- [x] Player closes browser mid-game, reopens, game resumes with correct board state
- [x] Player closes browser mid-game, reopens, opponent receives OPPONENT_RECONNECTED
- [x] Server is killed (`kill -9`) and restarted, in-progress game resumes correctly
- [x] Illegal move sent via WebSocket client (wscat): rejected with MOVE_REJECTED
- [x] Both players connected, one reaches timeout: GAME_OVER broadcast with reason TIMEOUT
- [x] Player resigns: opponent receives GAME_OVER with reason RESIGNATION

---

## Acceptance Criteria

Phase 1 is complete when all of the following are true. These are binary pass/fail.

| # | Criterion |
|---|-----------|
| 1 | Two players on different machines complete a full game from start to checkmate |
| 2 | Closing and reopening the browser mid-game (same player token) resumes the exact game state |
| 3 | Killing the server process and restarting it mid-game resumes the game correctly |
| 4 | An illegal move submitted directly via WebSocket is rejected; the board state does not change |
| 5 | A player whose clock reaches zero loses; this is detected server-side without client involvement |
| 6 | All tests pass with `go test -race ./...` |
| 7 | No goroutine leaks after a completed game (verify with `goleak` or pprof) |
| 8 | `make migrate-down && make migrate-up` succeeds cleanly (migrations are reversible) |
| 9 | Server logs all errors with gameID and relevant context; no bare `err.Error()` logs |
| 10 | A player cannot make a move when it is not their turn, regardless of what the client sends |

---

## Known Risks and Mitigations

| Risk | Likelihood | Mitigation |
|------|-----------|------------|
| Clock goroutine leak after game ends | Medium | `Clock.Stop()` called in all COMPLETED/ABANDONED transitions. Verified with goroutine count check. |
| Race condition: two messages from same client processed simultaneously | Low | Gorilla WebSocket read loop is single-goroutine per connection — messages are already serialized by the read loop. No additional locking needed for message processing order. |
| DB write fails mid-move pipeline | Medium | Move is rejected if DB write fails. Board state in-memory is not updated until after successful DB write. |
| Server restart loses clock state | Medium | `white_time_ms` and `black_time_ms` persisted to DB on every move and on disconnect. Loaded from DB on `RestoreActiveGames`. |
| notnil/chess threefold repetition detection | Low | notnil/chess handles this automatically. Covered by library tests, not our tests. |
| Token in query parameter logged in access logs | Low | Acceptable for Phase 1. Noted as technical debt. Move to first-message auth in Phase 3. |

---

## Technical Debt Introduced in Phase 1

| ID | Description | Acceptable Because | Fix By |
|----|-------------|-------------------|--------|
| TD-001 | Player token passed in URL query parameter (visible in logs) | No PII in token, dev environment only | Phase 3 |
| TD-002 | Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 simplification, not a real game | Phase 4 |
| TD-003 | No draw offer mechanism | Stalemate auto-detected | Phase 4 |
| TD-004 | Anonymous identity only (no real user accounts) | Phase 1 learning goal does not include auth | Phase 3 |
| TD-005 | Single time control (10+0 only) | Simplification for Phase 1 | Phase 4 |