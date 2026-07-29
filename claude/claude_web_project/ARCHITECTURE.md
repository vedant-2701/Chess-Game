# Architecture

This document describes how the chess server is built, why it is built that way, and what the contracts between components are. It reflects the current implemented state, not aspirational plans. When the architecture changes, this document is updated in the same commit.

> **Note on Phase 2:** Phase 2 (horizontal scaling — co-located sessions,
> Redis-backed ownership/liveness routing directory, resolve-then-connect
> connection flow) is now **fully implemented and E2E-verified**, not merely
> designed. Full reasoning trail: `phases/current/PHASE_2.md`,
> `DECISIONS_LOG_PHASE_2.md` ADR-021 through ADR-031. The sections below have
> been updated to describe the actual multi-instance system — System
> Overview, the `internal/api` endpoint list, WebSocket Connection Lifecycle,
> Game State Machine, Database Schema, and Dependency Graph all now reflect
> Phase 2 as built, not Phase 1 as originally documented here.

---

## System Overview (Phase 2 — current)

```
                    nginx Edge Proxy
           (round-robin REST; static /connect/{instanceLabel} map)
                    │                    │
         ┌──────────┘                    └──────────┐
         ▼                                           ▼
┌─────────────────────────┐              ┌─────────────────────────┐
│   Server Instance 1      │              │   Server Instance 2      │
│   api.WSHandler          │              │   api.WSHandler          │
│   api.GameHandler        │              │   api.GameHandler        │
│        │                 │              │        │                 │
│        ▼                 │              │        ▼                 │
│   game.Manager           │              │   game.Manager           │
│        │                 │              │        │                 │
│        ▼                 │              │        ▼                 │
│   game.GameRegistry      │              │   game.GameRegistry      │
│   ──► game.GameSession   │              │   ──► game.GameSession   │
│        │                 │              │        │                 │
└────────┼─────────────────┘              └────────┼─────────────────┘
         │                                           │
         └──────────────────┬────────────────────────┘
                             ▼
                  ┌──────────────────────┐
                  │  Redis (routing only) │  game:{id}→instanceID,
                  │                        │  instance_alive:{id}
                  └──────────────────────┘
                             │
                             ▼
                      PostgreSQL 16
              (shared source of truth for every instance)
```

**A game's live `GameSession` exists on exactly one instance at a time —
co-location, not state sync.** Redis holds only routing/coordination facts
(who owns this game, is that instance alive, and — as of ADR-030/031 —
nothing else; disconnect-grace-period state is in Postgres, not Redis, see
the Database Schema section below). There is never a second live
`GameSession` for the same game on a different instance simultaneously; the
entire Phase 2 design exists to make that structurally impossible rather
than to reconcile it if it happened. Full connection-flow detail
(resolve-then-connect, the two-token split, ownership claim/takeover):
`phases/current/PHASE_2.md`.

The original Phase 1 single-process diagram (no Redis, no nginx, no second
instance) remains accurate as a description of local/dev-mode single-instance
operation (`docker-up`, not `docker-up-cluster`) — the same binary serves
both modes, gated by whether a `RoutingDirectory` is configured
(`Manager.directory == nil` in single-instance mode skips every Redis call).

---

## System Overview (Phase 1 / single-instance mode)

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

**Single server, single process, no Redis.** Still exactly correct for local
development (`make docker-up`) and still the mode `restoreGame` (not
`hydrateGameSession`) serves. The scaling problem is what Phase 2 solves —
see above for the multi-instance system that now also exists.

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

Responsible for: Handling HTTP requests for game creation, joining, and
resolving, plus the WebSocket upgrade endpoint. This is the only layer that
depends on both `internal/ws` (for `*ws.Connection`, `ws.Registry`) and
`internal/game` (for `*game.Manager`) — see Dependency Graph below.
`internal/ws` itself has zero knowledge of games; `WSHandler` is what
bridges the two.

- `GameHandler` (`internal/api/game_handler.go`): `POST /games`,
  `POST /games/:id/join`, `GET /games/:id`, `GET /games/:id/resolve`,
  `GET /health`.
- `WSHandler` (`internal/api/ws_handler.go`, fully rewritten in Phase 2 for
  `ConnectClaims`): `GET /connect/{instanceLabel}`. Verifies the
  short-lived `ConnectClaims` token (not `PlayerClaims` — that's checked
  once, earlier, at resolve time), checks `claims.InstanceLabel` matches the
  URL parameter, upgrades to WebSocket, registers the connection into
  `ws.Registry`, and hands off to `game.Manager.HandleConnect` /
  `HandleMessage` / `HandleDisconnect`. The old Phase 1 route
  (`GET /ws/game/:id?token=<playerToken>`) no longer exists — removed
  entirely, not kept in parallel, per this project's "single correct code
  path" discipline (every connect or reconnect uses the identical
  resolve-then-connect path, ADR-022).

**Endpoints (current):**

```
POST /games
  Body: { "userID": "..." }
  Response: { "gameID": "uuid", "playerToken": "jwt", "color": "WHITE", "joinURL": "/game/uuid" }

POST /games/:id/join
  Body: { "userID": "..." }
  Response: { "gameID": "uuid", "playerToken": "jwt", "color": "BLACK" }
  Pure DB operation (ADR-028) — never touches GameRegistry/GameSession, no
  instance affinity, no resolve step involved.

GET /games/:id
  Response: { "gameID": "...", "status": "...", "currentFEN": "...", "outcome": null, "outcomeReason": null }
  status can be WAITING_FOR_PLAYER, ACTIVE, COMPLETED, ABANDONED, or ABORTED
  (ADR-029). Pure DB read, same affinity-free contract as join.

GET /games/:id/resolve
  Header: Authorization: Bearer <playerToken>
  Response: { "connectToken": "jwt", "instanceLabel": "...", "wsPath": "/connect/..." }
  Determines (or claims) the owning instance and mints a short-lived
  ConnectClaims token. See WebSocket Connection Lifecycle below.

GET /health
  Response: { "status": "ok" }

GET /connect/{instanceLabel}  (WebSocket upgrade)
  Query: ?token=<connectToken>
  Upgrades to WebSocket. gameID/userID/color come entirely from the
  verified ConnectClaims token, not the URL — the masked URL deliberately
  has no gameID path segment.
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

**ADDED (Phase 2, `DECISIONS_LOG_PHASE_2.md` ADR-029): `ABORTED`.** A game that never left `WAITING_FOR_PLAYER` — the creator connected, but nobody ever joined as the opponent, and the creator then disconnected — has no meaningful winner and was never actually contested. Distinct from `ABANDONED`: `outcome` and `outcome_reason` both stay `NULL`, not `DRAW`. Real chess platforms treat an opponent-never-showed-up game as void, not a scored draw — the same principle applies here. This closed a pre-existing gap (tracked as TD-P2-005, now resolved) where `WAITING_FOR_PLAYER` had no path to any terminal state at all. The trigger is the same abandonment timer as above, started from `HandleDisconnect` — the only difference is which branch `onAbandonTimeout` takes, gated on whether the game ever reached `ACTIVE`.

**ADDED (Phase 2, ADR-030/ADR-031): abandonment-timer continuity across instance failover.** The 60-second grace period above is armed via an in-process `time.Timer` (`Manager.abandonTimers`), which does not survive the owning instance dying. `white_disconnected_at`/`black_disconnected_at` columns (Database Schema below) persist the disconnect moment so a surviving instance can resume the correct remaining duration on hydration. If neither timestamp was ever written — the instance died before either individual disconnect could be observed, e.g. a crash while both players were still connected — the surviving instance assumes disconnected as of the hydration moment rather than assuming fine (ADR-031); a player who is in fact still connected self-cancels this defensive timer within their own reconnect. Not closed by this: a game where nobody ever calls `/resolve` again after a crash (tracked as TD-P2-006) — nothing runs if nobody triggers a hydrate.

**State definitions:**

| State | Description |
|-------|-------------|
| `WAITING_FOR_PLAYER` | Game created. White is connected or pending. Black has not joined. |
| `ACTIVE` | Both players connected. Moves are being played. |
| `COMPLETED` | Game has ended with a recorded outcome: checkmate, stalemate, resignation, timeout, OR single-player abandonment (opponent wins). |
| `ABANDONED` | Both players were disconnected simultaneously when the 60-second abandonment timer fired. Terminal, drawn outcome. |
| `ABORTED` | The creator connected but the opponent never joined at all before the creator disconnected and the 60-second timer fired. Terminal, void — no outcome, no outcome_reason. |

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
Client calls GET /games/:id/resolve with playerToken
        │
        ▼
api.GameHandler.Resolve → game.Manager.ResolveGame
        │
        ├── Owner alive in Redis → mint ConnectClaims{instanceLabel: owner}
        └── No owner / owner dead → claim ownership, hydrate session locally,
                                mint ConnectClaims{instanceLabel: this instance}
        │
        ▼
Client dials wss://.../connect/{instanceLabel}?token=<connectToken>
        │
        ▼
api.WSHandler upgrades HTTP to WebSocket
        │
        ▼
auth.VerifyConnectToken(token) → { gameID, userID, color, instanceLabel }
        │
        ├── Invalid/expired token → CloseMessage(1008), CONNECT_TOKEN_EXPIRED
        │    if expired — client should re-resolve, not retry the same URL
        │
        ▼
ws.Registry.Register(connID, conn)
        │
        ▼
game.Manager.HandleConnect(ctx, gameID, color, conn)
        │
        ├── registry.Get misses locally → GetOrHydrate fallback (safety net;
        │    the ordinary path already hydrated during resolve, above)
        │
        ├── Reconnection: game exists, player slot occupied
        │   → Replace old *Connection with new *Connection
        │   → Cancel any pending abandonment timer, clear disconnect timestamp
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
        ├── Start abandonment timer (60 seconds)
        └── Persist disconnect timestamp to Postgres (ADR-030, best-effort,
             detached context) — survives this process dying; a surviving
             instance resumes the correct remaining duration on hydrate
                │
                └── If player reconnects before timer: cancel timer, clear
                    timestamp, resume. If timer fires: outcome depends on
                    game status and opponent's connection state at that
                    moment — still WAITING_FOR_PLAYER → ABORTED (ADR-029);
                    ACTIVE, opponent connected → COMPLETED, opponent wins;
                    ACTIVE, opponent also disconnected → ABANDONED, drawn
```

Full connection-flow detail (the two-token split, ownership claim vs.
takeover, masked URLs): `phases/current/PHASE_2.md`.

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
                    CHECK (status IN ('WAITING_FOR_PLAYER','ACTIVE','COMPLETED','ABANDONED','ABORTED')),
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
    -- white_disconnected_at / black_disconnected_at (migration 004,
    -- DECISIONS_LOG_PHASE_2.md ADR-030/ADR-031): NULL unless that color
    -- currently has a pending abandonment grace period. Set on disconnect,
    -- cleared on reconnect. Persisted specifically because
    -- Manager.abandonTimers is pure per-process memory and does not survive
    -- the owning instance dying — a surviving instance reads these to
    -- resume (or immediately resolve) the grace period on hydration.
    white_disconnected_at TIMESTAMPTZ,
    black_disconnected_at TIMESTAMPTZ,
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
    ├── internal/game        (game sessions, move pipeline, routing directory client)
    │       ├── internal/chess    (move validation)
    │       ├── internal/store    (persistence)
    │       ├── internal/ws       (connection type only)
    │       └── github.com/redis/go-redis/v9 (RoutingDirectory — ownership/liveness
    │           only, gated by Manager.directory != nil; nil in single-instance
    │           mode, so internal/game has no HARD dependency on Redis being
    │           reachable, only a conditional one)
    │
    ├── internal/store       (PostgreSQL via pgx/v5)
    │       └── (no application dependencies)
    │
    ├── internal/auth        (JWT tokens — PlayerClaims and, as of Phase 2, ConnectClaims)
    │       └── (no application dependencies)
    │
    └── internal/chess       (notnil/chess wrapper)
            └── (no application dependencies)
```

Dependencies flow downward only. No circular imports. `internal/ws` does not know about `internal/game`. This is enforced by the Go compiler — not just a style convention: `internal/api` is the only package permitted to import both `internal/ws` and `internal/game`, since it is the one place both are genuinely needed (bridging an HTTP-upgraded WebSocket connection to a game session). `internal/game` is the only package that talks to Redis, and only for routing/ownership — no other package imports a Redis client.

---

## What Is Intentionally Not In This Architecture

- **No ORM**: Raw SQL via pgx/v5. ORMs hide query behavior and generate inefficient queries. You must know what your queries are.
- **No global state**: Everything is injected. No `var db *pgxpool.Pool` at package level.
- **No client-side game authority**: The client is a display terminal. It validates moves for UX only.
- **No match queue**: Players use shared links. Matchmaking is Phase 3.
- **No account system**: Anonymous userIDs generated client-side, signed into JWT. Full auth is post-Phase 1.
- **No cross-instance state synchronization**: Phase 2 deliberately routes to a single owning instance instead — there is never a second live `GameSession` for one game to synchronize with (`DECISIONS_LOG_PHASE_2.md` ADR-021). If a future phase needs genuine multi-writer state, that is a different, harder problem this project has not attempted.