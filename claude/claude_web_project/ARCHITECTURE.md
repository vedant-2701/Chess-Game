# Architecture

This document describes how the chess server is built, why it is built that way, and what the contracts between components are. It reflects the current implemented state, not aspirational plans. When the architecture changes, this document is updated in the same commit.

> **Note on Phase 2:** as of this revision, Phase 2 is fully designed
> (`phases/current/PHASE_2.md`, `DECISIONS_LOG_PHASE_2.md` ADR-021 through
> ADR-025) but not yet implemented — the system below is still accurately
> single-instance Phase 1. The one exception is the "EventBus Interface" section
> further down, which is corrected now because it currently asserts a specific
> forward plan (`RedisEventBus`) that has been superseded by design work, and an
> incorrect stated plan is worse than no stated plan. The rest of this document
> will be updated in the same commit as Phase 2's actual implementation, per this
> file's own rule above — not before.

---

## System Overview (Phase 1)

```
                          ┌─────────────────────────────────────────┐
                          │              Chess Server               │
                          │                                         │
  Browser (White) ───WS──►│  api.WSHandler                          │
                          │      │                                  │
  Browser (Black) ───WS──►│      ▼                                  │
                          │  game.Manager                           │
                          │      │                                  │
        REST API ─────────►  api.GameHandler                        │
  (POST /games)           │      │                                  │
  (POST /games/:id/join)  │      ▼                                  │
                          │  game.Registry ──► game.Session         │
                          │                        │                │
                          │                        ▼                │
                          │                   chess.Validator       │
                          │                        │                │
                          │                        ▼                │
                          │                   store.GameStore       │
                          │                   store.MoveStore       │
                          │                        │                │
                          └────────────────────────┼────────────────┘
                                                   │
                                                   ▼
                                            PostgreSQL 16
```

**Phase 1 is a single server, single process.** There is no Redis, no message queue, no external cache. All cross-goroutine communication happens via channels and mutexes within the process. This is intentional. The scaling problem surfaces in Phase 2.

---

## Layer Responsibilities

### `internal/ws` — WebSocket Infrastructure Layer

Responsible for: WebSocket connection lifecycle only. This layer knows nothing about chess, games, or players. It knows about connections, bytes, and goroutines.

- `Connection`: Holds a `*websocket.Conn`, a `send chan []byte`, and manages read/write goroutines independently. The write loop is the only goroutine that calls `conn.WriteMessage` (Gorilla is not concurrent-safe for writes).
- `Registry`: Thread-safe map of `connectionID → *Connection`. Handles registration, unregistration, and graceful shutdown with the snapshot-then-release pattern to avoid deadlock.

**This layer does not know what a game is.** It knows how to move bytes. Everything else is the application layer's concern.

**Correction (2026-07-02):** An earlier draft of this document and of `PHASE_1.md` placed the WebSocket upgrade `Handler` inside this package, holding a `*game.Manager` field directly. That is not possible as written: `internal/game` already imports `internal/ws` (for `*ws.Connection`), so `internal/ws` importing `internal/game` back would be a circular import — rejected by the Go compiler, not just a style violation. The upgrade handler lives in `internal/api` (`WSHandler`) instead; see that section below and the Dependency Graph. This keeps the statement above literally true: `internal/ws` genuinely has zero dependency on `internal/game`, not just "ignorant of it" by convention.

### `internal/game` — Game Application Layer

Responsible for: Game lifecycle, session management, move routing, player identity, reconnection.

- `GameSession`: The core struct. Contains the board state (via chess.Game), player connections (two `*ws.Connection` pointers), game state machine, and clocks.
- `GameRegistry`: Thread-safe map of `gameID → *GameSession`. The bridge between a connection and a game.
- `Manager`: The orchestrator. Receives raw messages from the WebSocket layer, routes them to the correct `GameSession`, and coordinates responses.
- `move.go`: The move processing pipeline. Every move goes through: parse → turn check → legality validate → persist → broadcast → outcome check.

### `internal/chess` — Chess Domain Layer

Responsible for: Wrapping `notnil/chess` library. Nothing else.

This layer exists as a seam so the chess library is not imported directly throughout the codebase. If `notnil/chess` is ever replaced, only this package changes.

The chess layer defines four operations:

- **ValidateAndApply**: accepts a current game state and a move in Standard Algebraic Notation. Returns the updated game state if the move is legal, or an error if it is not. The input game state is never mutated — the return value is the new state.
- **DetectOutcome**: accepts a current game state. Returns whether the game has ended, who won or whether it is a draw, and the reason (checkmate, stalemate, etc.). Returns a "no outcome" result when the game is still in progress.
- **CurrentFEN**: accepts a game state and returns the current board position as a FEN string.
- **MoveHistory**: accepts a game state and returns the complete ordered list of moves played in SAN.

### `internal/store` — Persistence Layer

Responsible for: All database interaction. Returns domain types, not database rows.

- `GameStore`: CreateGame, GetGame, UpdateGameStatus, UpdateCurrentFEN, GetActiveGames
- `MoveStore`: SaveMove, GetMovesForGame

**Rule:** No SQL outside the store layer. Every other layer calls store methods. Game logic never constructs a query.

### `internal/auth` — Authentication Layer

Responsible for: Signing and verifying JWT player tokens. Nothing else.

Player tokens encode: `{ gameID, userID, color, iat, exp }`. They are signed with HMAC-SHA256. They are not stored in the database. Verification is stateless.

Two token types exist:
- `playerToken`: Scoped to a specific game and color. Used for WebSocket authentication and reconnection.
- (Phase 3+) `userToken`: Persistent identity across games. Not in Phase 1.

### `internal/api` — HTTP API Layer

Responsible for: Handling HTTP requests for game creation and joining, and the WebSocket upgrade endpoint. This is the only layer that depends on both `internal/ws` (for `*ws.Connection`, `ws.Registry`) and `internal/game` (for `*game.Manager`) — see Dependency Graph below. `internal/ws` itself has zero knowledge of games; `WSHandler` is what bridges the two.

- `GameHandler` (`internal/api/game_handler.go`, Step 12): `POST /games`, `POST /games/:id/join`, `GET /games/:id`, `GET /health`.
- `WSHandler` (`internal/api/ws_handler.go`, Step 11): `GET /ws/game/:id`. Verifies the player token, upgrades to WebSocket, registers the connection into `ws.Registry`, and hands off to `game.Manager.HandleConnect` / `HandleMessage` / `HandleDisconnect`.

**Endpoints (Phase 1):**

```
POST /games
  Body: {}
  Response: { "gameID": "uuid", "playerToken": "jwt", "gameURL": "/game/uuid" }

POST /games/:id/join
  Body: {}
  Response: { "gameID": "uuid", "playerToken": "jwt" }

GET /games/:id
  Response: { "gameID": "...", "status": "...", "currentFEN": "...", "outcome": null, "outcomeReason": null }

GET /health
  Response: { "status": "ok" }
  
GET /ws/game/:id  (WebSocket upgrade)
  Query: ?token=<playerToken>
  Upgrades to WebSocket
```

---

## Game State Machine

```
         POST /games
              │
              ▼
         WAITING_FOR_PLAYER
              │
              │ (Second player connects via WebSocket)
              ▼
            ACTIVE ───────────────────────────────────────────┐
              │                                               │
              │ (Checkmate / Stalemate / Timeout detected)    │
              │ (Player resigns)                              │
              │ (Both players abandon)                        │
              ▼                                               │
           COMPLETED                                    (Player disconnects,
                                                         reconnects within window)
                                                              │
                                                         ACTIVE (resumed)
```

**State transitions are the only place game status is changed.** No code outside `GameSession` is allowed to change game state directly.

**CORRECTED (post-Step-10, see DECISIONS_LOG_PHASE_1.md ADR-015):** The diagram above is simplified and predates a correction to abandonment semantics. As drawn, it implies `ABANDONED` is reachable only when both players disconnect. The authoritative, corrected behavior — detailed in full in PHASE_1.md's Game State Machine section — is:

- **Single-player disconnect, opponent stays connected, 60s elapse without reconnection:** the disconnected player loses by abandonment. Transition is `ACTIVE → COMPLETED`, with `outcome` set to the connected player's color and `outcome_reason: ABANDONED`. This is the common case.
- **Both players disconnected simultaneously when the 60s timer fires:** transition is `ACTIVE → ABANDONED`, with `outcome: DRAW` and `outcome_reason: ABANDONED`.

`ABANDONED` status is reserved exclusively for the both-disconnected case. A single-player abandonment is recorded as `COMPLETED`, distinguishing it from a drawn `ABANDONED` game by the `status` field, not the `outcome_reason` field (both share `outcome_reason: ABANDONED`).

**State definitions:**

| State | Description |
|-------|-------------|
| `WAITING_FOR_PLAYER` | Game created. White is connected or pending. Black has not joined. |
| `ACTIVE` | Both players connected. Moves are being played. |
| `COMPLETED` | Game has ended with a recorded outcome: checkmate, stalemate, resignation, timeout, OR single-player abandonment (opponent wins). |
| `ABANDONED` | Both players were disconnected simultaneously when the 60-second abandonment timer fired. Terminal, drawn outcome. |

---

## Move Processing Pipeline

Every move goes through this exact sequence. If any step fails, the move is rejected and the error is returned to the client.

```
Client sends: { "type": "MOVE", "san": "e4" }
        │
        ▼
1. ws.Connection.readLoop receives raw bytes
        │
        ▼
2. game.Manager routes message by type → MoveHandler
        │
        ▼
3. Validate it is this player's turn
   (wrong turn → MOVE_REJECTED, stop)
        │
        ▼
4. chess.Validator.ValidateAndApply(currentGame, "e4")
   (illegal move → MOVE_REJECTED, stop)
        │
        ▼
5. store.MoveStore.SaveMove(ctx, gameID, moveNumber, "e4", fenAfter)
   (DB error → MOVE_REJECTED, stop — move is NOT applied)
        │
        ▼
6. store.GameStore.UpdateCurrentFEN(ctx, gameID, fenAfter)
        │
        ▼
7. Advance clock: stop mover's clock, start opponent's clock
        │
        ▼
8. chess.Validator.DetectOutcome(updatedGame)
        │
        ├── Outcome detected → broadcast GAME_OVER to both players
        │                     update game status to COMPLETED in DB
        │
        └── No outcome → broadcast MOVE_APPLIED to both players
                         { san, fen, turn, whiteTimeMs, blackTimeMs }
```

**Critical design decision:** Step 5 (persist) happens before step 7 (broadcast). A move is not applied to the game state until it is persisted. If the database write fails, the move is rejected as if it never happened. The client must not assume a move is accepted until it receives `MOVE_APPLIED`.

---

## WebSocket Connection Lifecycle

```
Client connects to /ws/game/:id?token=<jwt>
        │
        ▼
api.WSHandler upgrades HTTP to WebSocket
        │
        ▼
auth.VerifyPlayerToken(token) → { gameID, userID, color }
        │
        ├── Invalid token → CloseMessage(1008 Policy Violation), return
        │
        ▼
ws.Registry.Register(connID, conn)
        │
        ▼
game.Manager.HandleConnect(ctx, gameID, color, conn)  // internally: registry.Get + session.RegisterConnection/ReplaceConnection
        │
        ├── Game not found → CloseMessage, unregister from ws.Registry
        │
        ├── Reconnection: game exists, player slot occupied
        │   → Replace old *Connection with new *Connection
        │   → Send GAME_STATE to reconnecting player
        │   → Send OPPONENT_RECONNECTED to other player
        │
        └── New join: player slot empty
            → Set *Connection in GameSession
            → If both players now connected: transition WAITING → ACTIVE
            → Send GAME_STATE to both players
            → Start clock goroutine

[Connection is live — read/write loops running]

Client disconnects (read loop returns error or close frame)
        │
        ▼
ws.Registry.Unregister(connID)
        │
        ▼
game.Manager.HandleDisconnect(gameID, color)
        │
        ├── Set player's *Connection to nil in GameSession
        ├── Notify opponent: OPPONENT_DISCONNECTED
        └── Start abandonment timer (e.g. 60 seconds)
                │
                └── If player reconnects before timer: cancel timer, resume
                    If timer fires: outcome depends on opponent's connection
                    state at that moment (see Game State Machine correction
                    above) — opponent connected → COMPLETED, opponent wins;
                    opponent also disconnected → ABANDONED, drawn
```

---

## Two-Registry Architecture

A critical design decision: there are two separate registries with separate concerns.

```
ws.Registry                          game.GameRegistry
─────────────────────────────        ──────────────────────────────────
Key: connectionID (string)           Key: gameID (UUID string)
Value: *ws.Connection                Value: *game.GameSession

Knows: "conn-abc123 exists"          Knows: "game xyz has White=conn-abc123
Doesn't know: which game                    and Black=conn-def456"

Lifecycle: connection lifetime       Lifecycle: game lifetime (longer)
```

When a player reconnects, their connectionID changes. The `GameSession` holds pointers to `*Connection` objects. On reconnection, the `GameSession` replaces its old pointer with the new connection. The `ws.Registry` always reflects current live connections. The `game.GameRegistry` reflects current game state.

---

## EventBus Interface

**Corrected — see note at top of this document.** This section originally read
"(Phase 2 Seam)" and stated that Phase 2 would inject a `RedisEventBus` in place
of `LocalEventBus`. That plan is superseded: Phase 2's design (co-located
sessions, routed via a Redis-backed ownership/liveness directory — see
`phases/current/PHASE_2.md`) never has two processes holding live state for the
same game simultaneously, so there is nothing for a cross-instance event bus to
synchronize. `LocalEventBus` is the permanent implementation, not a Phase-1
placeholder. Full reasoning: `DECISIONS_LOG_PHASE_2.md` ADR-021, which supersedes
the Phase 2 half of ADR-010 below (ADR-010 itself is not edited, per this
project's append-only ADR discipline — it is superseded, not rewritten).

Phase 1 does not use Redis. Redis is introduced in Phase 2, but not for the
EventBus — it is used solely as a routing directory (which instance currently
owns a given game), a different component with a different interface,
unrelated to `EventBus`/`GameEvent` below.

```go
// internal/game/eventbus.go

type GameEvent struct {
    GameID  string
    Type    string
    Payload []byte
}

type EventBus interface {
    Publish(ctx context.Context, event GameEvent) error
    Subscribe(ctx context.Context, gameID string) (<-chan GameEvent, func(), error)
}

// Permanent implementation — not a Phase-1 placeholder (see correction above)
type LocalEventBus struct {
    mu          sync.RWMutex
    subscribers map[string][]chan GameEvent
}
```

`LocalEventBus` is used in Phase 1 and remains in use indefinitely. There is no
`RedisEventBus` — see the correction above for why.

---

## Database Schema

```sql
-- Minimal anonymous user identity
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Game record
CREATE TABLE games (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    status          TEXT NOT NULL DEFAULT 'WAITING_FOR_PLAYER'
                    CHECK (status IN ('WAITING_FOR_PLAYER','ACTIVE','COMPLETED','ABANDONED')),
    player_white_id UUID REFERENCES users(id),
    player_black_id UUID REFERENCES users(id),
    current_fen     TEXT NOT NULL DEFAULT 'rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1',
    white_time_ms   INTEGER NOT NULL DEFAULT 600000,  -- 10 minutes in ms
    black_time_ms   INTEGER NOT NULL DEFAULT 600000,
    outcome         TEXT CHECK (outcome IN ('WHITE','BLACK','DRAW')),
    outcome_reason  TEXT CHECK (outcome_reason IN (
                        'CHECKMATE','STALEMATE','RESIGNATION',
                        'TIMEOUT','DRAW_AGREEMENT','ABANDONED'
                    )),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Full move history
CREATE TABLE moves (
    id          BIGSERIAL PRIMARY KEY,
    game_id     UUID NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    move_number INT NOT NULL,
    color       TEXT NOT NULL CHECK (color IN ('WHITE','BLACK')),
    san         TEXT NOT NULL,     -- Standard Algebraic Notation: "e4", "Nf3", "O-O"
    fen_after   TEXT NOT NULL,     -- Board state after this move (for replay)
    played_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_moves_game_move ON moves(game_id, move_number);
CREATE INDEX idx_moves_game_id ON moves(game_id);
CREATE INDEX idx_games_status ON games(status) WHERE status IN ('WAITING_FOR_PLAYER','ACTIVE');
```

**Why `current_fen` on games AND `fen_after` on moves:**
- `games.current_fen`: Current board position in O(1). Used for reconnection state delivery and server restart recovery. If absent, resuming a game requires replaying all moves.
- `moves.fen_after`: Full history for game analysis, PGN export, and threefold repetition detection. Used in Phase 4.

**Why `white_time_ms` and `black_time_ms` on games:**
Persistent clock state. On server restart, the remaining time for both players is loaded from the database so the clock can resume accurately rather than resetting.

---

## Dependency Graph

```
cmd/server/main.go
    │
    ├── internal/api         (chi router, HTTP handlers, WS upgrade)
    │       ├── internal/game (game.Manager)
    │       └── internal/ws   (ws.Connection, ws.Registry — types only, not the reverse)
    │
    ├── internal/ws          (WebSocket infrastructure)
    │       └── (no application dependencies)
    │
    ├── internal/game        (game sessions, move pipeline)
    │       ├── internal/chess    (move validation)
    │       ├── internal/store    (persistence)
    │       └── internal/ws       (connection type only)
    │
    ├── internal/store       (PostgreSQL via pgx/v5)
    │       └── (no application dependencies)
    │
    ├── internal/auth        (JWT tokens)
    │       └── (no application dependencies)
    │
    └── internal/chess       (notnil/chess wrapper)
            └── (no application dependencies)
```

Dependencies flow downward only. No circular imports. `internal/ws` does not know about `internal/game`. This is enforced by the Go compiler — not just a style convention: `internal/api` is the only package permitted to import both `internal/ws` and `internal/game`, since it is the one place both are genuinely needed (bridging an HTTP-upgraded WebSocket connection to a game session).

---

## What Is Intentionally Not In This Architecture

- **No ORM**: Raw SQL via pgx/v5. ORMs hide query behavior and generate inefficient queries. You must know what your queries are.
- **No global state**: Everything is injected. No `var db *pgxpool.Pool` at package level.
- **No client-side game authority**: The client is a display terminal. It validates moves for UX only.
- **No horizontal scaling in Phase 1**: A single server. The scaling problem is kept for Phase 2 so it is understood before it is solved.
- **No match queue**: Players use shared links. Matchmaking is Phase 3.
- **No account system**: Anonymous userIDs generated client-side, signed into JWT. Full auth is post-Phase 1.