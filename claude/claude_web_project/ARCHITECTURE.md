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

> **Note on Phase 3 (added 2026-08-08, updated 2026-08-17):** Phase 3
> (matchmaking) is now **implemented and E2E-verified** — all 6 integration
> scenarios run against the live cluster (`e2e-phase3.sh`), full `go
> build`/`vet`/`test -race`/`test -tags integration -race` gate clean. Full
> reasoning trail: `DECISIONS_LOG_PHASE_3.md` ADR-032 through ADR-044,
> `phases/current/PHASE_3.md`. The "Matchmaking" section near the end of
> this document has been rewritten to describe the actual built system, not
> a forward-looking design record — same treatment this document gave
> Phase 2 once that phase was implemented. Two smaller additions from this
> same pass, also now real: username/password registration/login
> (`DECISIONS_LOG_PHASE_3.md` ADR-043, `POST /users`/`POST /login`), and a
> universally-scoped `ACTIVE→ABORTED` first-move grace period
> (ADR-041) that changes Phase 1/2 behavior retroactively — see the Game
> State Machine section below. Phase 3's remaining open items (`ARCHITECTURE.md`
> reconciliation, this pass; the Phase 1/2 regression pass; `CLAUDE.md`) are
> tracked in `phases/current/PHASE_3.md`'s Step 7/8 checklist, not here.

---

## System Overview (Phase 2/3 — current)

```
                    nginx Edge Proxy
   (round-robin REST/matchmaking; static /connect/{instanceLabel} map)
           │            │                     │            │
     /games,/users  /connect      /matchmaking (Phase 3, ADR-035)
     /login          │                     │
         │            │                     │            │
         ▼            ▼                     ▼            ▼
┌─────────────────────────┐              ┌─────────────────────────┐   ┌──────────────────────────┐
│   Server Instance 1      │              │   Server Instance 2      │   │  matchmaking-service      │
│   api.WSHandler          │              │   api.WSHandler          │   │  (own container, Phase 3) │
│   api.GameHandler        │              │   api.GameHandler        │   │  mmsvc.Handler (HTTP/SSE) │
│   api.AuthHandler        │              │   api.AuthHandler        │   │  mmsvc.ReportServer (gRPC)│
│        │                 │              │        │                 │   │  mmsvc.Sweep (queue TTL)  │
│        ▼                 │              │        ▼                 │   └──────────┬────────────────┘
│   game.Manager           │              │   game.Manager           │              │
│        │                 │              │        │                 │              │ gRPC
│        ▼                 │              │        ▼                 │◄─────────────┘ (MatchReportService,
│   game.GameRegistry      │              │   game.GameRegistry      │                shared-secret
│   ──► game.GameSession   │              │   ──► game.GameSession   │                interceptor, ADR-033)
│        │                 │              │        │                 │
│        ▼                 │              │        ▼                 │
│ internal/matchmaking     │              │ internal/matchmaking     │
│ (pairing loop, ZPOPMIN)  │              │ (pairing loop, ZPOPMIN)  │
└────────┼─────────────────┘              └────────┼─────────────────┘
         │                                           │
         └──────────────────┬────────────────────────┘
                             ▼
                  ┌───────────────────────────────┐
                  │  Redis (routing + matchmaking)  │  game:{id}→instanceID,
                  │                                  │  instance_alive:{id},
                  │                                  │  matchmaking:queue:10+0 (ZSET),
                  │                                  │  matchmaking_active_game:{userID},
                  │                                  │  matchmaking_result:{userID},
                  │                                  │  reported:{gameID}
                  └───────────────────────────────┘
                             │
                             ▼
                      PostgreSQL 16
              (shared source of truth for every chess-server instance —
               matchmaking-service never connects to it, ADR-032)
```

**A game's live `GameSession` exists on exactly one instance at a time —
co-location, not state sync.** Redis holds routing/coordination facts (who
owns this game, is that instance alive) plus, as of Phase 3, the
matchmaking queue and its supporting keys — a new namespace on the same
Redis instance, not new infrastructure (`DECISIONS_LOG_PHASE_3.md`
ADR-032). There is never a second live `GameSession` for the same game on a
different instance simultaneously; the entire Phase 2 design exists to make
that structurally impossible rather than to reconcile it if it happened.
Full connection-flow detail (resolve-then-connect, the two-token split,
ownership claim/takeover): `phases/current/PHASE_2.md`. Full matchmaking
flow (queue, pairing, SSE notification, the two connect paths): the
Matchmaking section below.

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

Responsible for: signing/verifying JWTs, and (as of Phase 3, ADR-043)
hashing/verifying passwords. Nothing else — no HTTP, no persistence; those
are `internal/api`'s and `internal/store`'s jobs respectively.

Four JWT claim shapes exist, one signing secret shared across all of them
(this codebase's consistent one-secret/multiple-claim-shapes convention):

- `PlayerClaims{GameID, UserID, Color}`: long-lived (24h). Minted by
  `CreateGame`/`JoinGame`/`CreateMatchedGame`. Used for WebSocket
  reconnection authentication indirectly — via `/resolve`, never dialed
  directly — and as `ActiveGameMarker.PlayerToken`, the credential a client
  falls back to via `/resolve` when a fresher token has expired.
- `ConnectClaims{GameID, UserID, Color, InstanceLabel}`: short-lived
  (`ConnectClaimsTTL`, default 30s). Minted in two places, both inside
  `internal/game.Manager`, sharing one helper (`signConnectToken`,
  `DECISIONS_LOG_PHASE_3.md` ADR-044): `ResolveGame` (the ordinary
  reconnect path) and `CreateMatchedGame` (the initial post-match
  connection, ADR-044 — safe to mint here because the instance minting it
  is synchronously about to claim ownership of the game, so there is zero
  staleness window). The only claim shape that includes `InstanceLabel`,
  which is what `WSHandler` checks against the URL.
- `MatchmakingClaims{UserID}` (Phase 3, ADR-036): scopes a
  `POST /matchmaking/queue` session (`GET /matchmaking/stream`/`status`,
  `DELETE /matchmaking/queue`). `MatchmakingClaimsTTL` (default 60s) is
  deliberately sized to safely exceed `MATCHMAKING_QUEUE_TIMEOUT_SECONDS` —
  validated at `matchmaking-service` startup, not just hoped for
  (`DECISIONS_LOG_PHASE_3.md` §18.5).

**Password hashing (Phase 3, ADR-043):** `HashPassword`/`VerifyPassword`
(`internal/auth/password.go`), bcrypt, default cost. Backs the minimal
username/password registration described in the Authentication
(username/password) section below — not a general session/auth layer,
deliberately.

Token TTLs (`DefaultConnectClaimsTTL`, `DefaultMatchmakingClaimsTTL`) are
only fallback defaults in `internal/auth` — the enforced values are
injected constructor fields on `internal/game.Manager`/`internal/mmsvc.Handler`
respectively (`CODING_GUIDELINES.md` §5: no package-level mutable state),
resolved from `CONNECT_CLAIMS_TTL_SECONDS`/`MATCHMAKING_CLAIMS_TTL_SECONDS`
at each binary's startup.

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
- `AuthHandler` (`internal/api/auth_handler.go`, added Phase 3,
  `DECISIONS_LOG_PHASE_3.md` ADR-043, fixes TD-P3-007): `POST /users`,
  `POST /login`. See the Authentication Layer section below for the full
  scope — deliberately minimal, returns a bare `userID`, no token, no
  session, no expiration.
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

**Envelope correction (found during Phase 3 pre-planning reconciliation,
fixed here):** the responses below now show the actual wire format —
confirmed against `internal/api/response.go`'s `writeData` (every response
wrapped in `{"data": ...}`, `GET /health` the sole exception). These
examples previously showed the bare inner payload only; that was always an
inaccurate representation of what these endpoints actually return, not a
deliberate shorthand — fixed here rather than left inconsistent with
`phases/current/PHASE_3.md`, which received the same correction first.

```
POST /games
  Body: { "userID": "..." }
  Response: { "data": { "gameID": "uuid", "playerToken": "jwt", "color": "WHITE", "joinURL": "/game/uuid" } }

POST /games/:id/join
  Body: { "userID": "..." }
  Response: { "data": { "gameID": "uuid", "playerToken": "jwt", "color": "BLACK" } }
  Pure DB operation (ADR-028) — never touches GameRegistry/GameSession, no
  instance affinity, no resolve step involved.

GET /games/:id
  Response: { "data": { "gameID": "...", "status": "...", "currentFEN": "...", "outcome": null, "outcomeReason": null } }
  status can be WAITING_FOR_PLAYER, ACTIVE, COMPLETED, ABANDONED, or ABORTED
  (ADR-029). Pure DB read, same affinity-free contract as join.

GET /games/:id/resolve
  Header: Authorization: Bearer <playerToken>
  Response: { "data": { "connectToken": "jwt", "instanceLabel": "...", "wsPath": "/connect/..." } }
  Determines (or claims) the owning instance and mints a short-lived
  ConnectClaims token. See WebSocket Connection Lifecycle below.

POST /users  (Phase 3, ADR-043)
  Body: { "username": "...", "password": "..." }
  Response 201: { "data": { "userID": "uuid" } }
  Response 409: { "error": { "code": "USERNAME_TAKEN", "message": "..." } }
  Creates a new user identified by username+password, returns its generated
  userID — used exactly like an anonymously-generated one everywhere else
  in this API (POST /games, POST /matchmaking/queue, etc.). No token, no
  session, no expiration, by deliberate design — see Authentication Layer
  below.

POST /login  (Phase 3, ADR-043)
  Body: { "username": "...", "password": "..." }
  Response 200: { "data": { "userID": "uuid" } }
  Response 401: { "error": { "code": "INVALID_CREDENTIALS", "message": "..." } }
  Recovers the userID Register originally handed out — e.g. from a new
  device. An unknown username gets the IDENTICAL response as a wrong
  password (same code, same status) — deliberate, closes a
  username-enumeration side channel.

GET /health
  Response: { "status": "ok" }
  The one documented exception to the data-envelope rule — confirmed
  directly against response.go, not an oversight here.

GET /connect/{instanceLabel}  (WebSocket upgrade)
  Query: ?token=<connectToken>
  Upgrades to WebSocket. gameID/userID/color come entirely from the
  verified ConnectClaims token, not the URL — the masked URL deliberately
  has no gameID path segment. Not a JSON response at all — envelope rule
  does not apply.
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

**ADDED (Phase 3, `DECISIONS_LOG_PHASE_3.md` ADR-041): first-move grace period — `ACTIVE → ABORTED`, universally scoped.** A game that reaches `ACTIVE` (both players connected) but never receives a first move, or receives White's first move with no reply from Black, previously had no termination mechanism at all — and the existing disconnect-triggered abandonment timer would in fact score it misleadingly (a `COMPLETED` win or `ABANDONED` draw for a game with zero real chess played) on the rare occasion it did fire. Closed by a move-count-gated timer, armed at the same points the abandonment timer already is:

- After `ACTIVE` with **zero moves played**: a timer arms for Black's first move. If it fires, `ACTIVE → ABORTED` (reusing the same terminal state `WAITING_FOR_PLAYER → ABORTED` already uses — void, no `outcome`/`outcome_reason`, not a scored draw).
- After **exactly one move played** (White's first move, Black hasn't replied): the timer re-arms for Black's reply specifically, retired once move #2 lands.
- After **two or more moves**: this mechanism retires permanently for that game — the ordinary disconnect-triggered abandonment timer is the only termination path from then on, exactly as before this ADR.

**This is a retroactive behavior change to already-shipped Phase 1/2 code, not a matchmaking-only addition** — the underlying defect (a stalled game with zero real chess played having no termination path) was never specific to how the game was created. Implementation: `internal/game/move.go`'s `MoveProcessor.onMovePersisted` hook (mirrors the existing `Clock→Manager` callback pattern) notifies `Manager.onMovePersisted` after every persisted move, which arms/retires this timer based on the game's current move count. **Flagged explicitly for the Phase 1/2 regression pass** (`phases/current/PHASE_3.md` acceptance criterion #8) — this is the one change in this phase most likely to surface as an unexpected new termination path in an existing Phase 1/2 test scenario that plays zero or one moves before asserting on game state.

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
-- Minimal anonymous user identity, PLUS optional username/password
-- (Phase 3, DECISIONS_LOG_PHASE_3.md ADR-043 — fixes TD-P3-007). The two
-- identity paths coexist: CreateOrGetUser (anonymous, client-supplied ID,
-- unchanged since Phase 1) and CreateUserWithCredentials (registered,
-- server-generated UUID v7). username/password_hash nullable together
-- (CHECK constraint enforces both-or-neither) — an anonymous user has
-- neither.
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT UNIQUE,
    password_hash TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((username IS NULL) = (password_hash IS NULL))
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
    -- matchmaking_request_id (migration 005, Phase 3, ADR-034): NULL unless
    -- this game was created via CreateMatchedGame. UUID v4 (a
    -- correlation/idempotency token generated once per pairing attempt and
    -- reused across retries of that attempt), not v7 like id above — this
    -- is not a DB-primary-key generation, it's a caller-supplied dedup key.
    -- UNIQUE + ON CONFLICT DO NOTHING is what makes CreateMatchedGame safe
    -- to retry after an ambiguous failure.
    matchmaking_request_id UUID UNIQUE,
    -- activated_at (migration 006, Phase 3, ADR-041/042): NULL until the
    -- game transitions WAITING_FOR_PLAYER → ACTIVE. Set atomically with
    -- that transition (GameStore.ActivateGame). Anchors ADR-041's
    -- move-count-gated first-move-grace-period timer and ADR-042's
    -- matched-opponent-never-connected timeout — both need to know exactly
    -- when a game actually went live, not just when it was created.
    activated_at    TIMESTAMPTZ,
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
    │       ├── internal/store (AuthHandler's UserStore)
    │       ├── internal/auth  (AuthHandler's password hashing)
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
    ├── internal/matchmaking (Phase 3 — pairing loop, chess-server side only)
    │       ├── internal/game     (Manager.CreateMatchedGame, MatchedGameTokens)
    │       ├── internal/rpc      (shared-secret gRPC client interceptor)
    │       ├── proto/matchmakingv1 (generated gRPC client stubs)
    │       └── github.com/redis/go-redis/v9 (reuses main.go's existing client —
    │           implementation-time decision, not requiring its own ADR; see
    │           NewPairingLoop's signature, which only accepts an
    │           already-connected client, never constructs its own)
    │
    ├── internal/store       (PostgreSQL via pgx/v5)
    │       └── (no application dependencies)
    │
    ├── internal/auth        (JWT tokens — PlayerClaims, ConnectClaims,
    │                        MatchmakingClaims — plus bcrypt password hashing, Phase 3)
    │       └── (no application dependencies)
    │
    └── internal/chess       (notnil/chess wrapper)
            └── (no application dependencies)

cmd/matchmaking-service/main.go   (Phase 3 — separate binary, separate container)
    │
    └── internal/mmsvc       (HTTP/SSE handlers, gRPC server, queue-timeout sweep)
            ├── internal/auth     (MatchmakingClaims sign/verify — the only
            │                       internal/auth capability this binary uses;
            │                       ConnectClaims/PlayerClaims are minted by
            │                       chess-server and relayed here as opaque strings,
            │                       never parsed or reconstructed)
            ├── internal/rpc      (shared-secret gRPC server interceptor)
            ├── proto/matchmakingv1 (generated gRPC server stubs)
            └── github.com/redis/go-redis/v9 (own client, internal/mmsvc.NewRedisClient
                — deliberately duplicated from internal/game's rather than
                imported, so this binary never depends on internal/game/
                internal/store/internal/chess/internal/ws at all — ADR-032's
                "matchmaking-service never talks to GameStore/GameRegistry"
                boundary, enforced at the import graph level, not just by
                convention)
```

Dependencies flow downward only. No circular imports. `internal/ws` does
not know about `internal/game`. This is enforced by the Go compiler — not
just a style convention: `internal/api` is the only package permitted to
import both `internal/ws` and `internal/game`, since it is the one place
both are genuinely needed (bridging an HTTP-upgraded WebSocket connection
to a game session). `internal/game` is the only chess-server-side package
that talks to Redis for routing/ownership; `internal/matchmaking` is a
second chess-server-side Redis client, but for the matchmaking queue only
(a different key namespace, not routing/ownership), and reuses `main.go`'s
already-connected client rather than opening its own connection.
`matchmaking-service` is a genuinely separate binary with its own `main.go`
and its own Redis client — it shares no Go package import, and no process,
with chess-server; the gRPC contract (`proto/matchmakingv1`) is the only
link between the two binaries.

---

## Matchmaking (Phase 3 — Implemented, 2026-08-17)

> This section describes the built system — `DECISIONS_LOG_PHASE_3.md`
> ADR-032 through ADR-044, all six `e2e-phase3.sh` integration scenarios
> verified against the live cluster. Superseded framing: this section
> previously described a design-decided-not-yet-implemented state; that
> framing no longer applies. See `phases/current/PHASE_3.md` for the full
> checklist and `phases/current/PHASE_3_DESIGN_NOTES.md` for the complete
> reasoning trail, including §18's connect-flow redesign (ADR-044).

### Component overview

```
                    nginx Edge Proxy (shared, existing — ADR-035)
         │                              │                    │
         │ /games, /connect             │ /matchmaking       │
         ▼                              ▼                    │
  chess-server instances          matchmaking-service            │
  (existing, Phase 1/2)           (own container, own binary)    │
         │                              │                    │
         │ internal/matchmaking         │ internal/mmsvc     │
         │ pairing loop: ZPOPMIN         │ Handler (HTTP/SSE) │
         │                              │ Hub (SSE fan-out,   │
         │                              │   userID→chan, M=1) │
         │                              │ ReportServer (gRPC) │
         │                              │ Sweep (queue TTL)   │
         ▼                              ▼                    │
     Redis (existing instance, new namespace)
       matchmaking:queue:10+0 (ZSET)
       matchmaking_active_game:{userID}  (PlayerToken lease, chess-server-written)
       matchmaking_result:{userID}       (ReportServer/Sweep-written, 5min TTL)
       reported:{gameID}                 (ReportMatchCreated dedup, 5min TTL)
         │
         ▼
     PostgreSQL (existing — matchmaking_request_id + activated_at columns on games)

  chess-server ── gRPC (MatchReportService, shared-secret interceptor) ──▶ matchmaking-service
```

**Key structural facts, all confirmed as-built:**
- `matchmaking-service` never picks or assigns a pair, and never talks to
  `GameStore`, `GameRegistry`, Postgres, or the Redis ownership keys —
  confirmed at the import-graph level (Dependency Graph above), not just by
  convention: `cmd/matchmaking-service` has no transitive dependency on
  `internal/game`/`internal/store` at all.
- chess-server instances never talk to each other directly, and never hold
  or know about any SSE connection. `ZPOPMIN`'s atomicity is the entire
  cross-instance coordination mechanism for pairing — verified under real
  concurrent load, not just `-race`-simulated (`e2e-phase3.sh scenario4`:
  20 players queued rapidly against both live instances, exactly 10 matched
  games, zero double-matches).
- The two services share the existing Redis instance (new key namespace,
  not new infrastructure) and the existing nginx edge proxy (`location
  /matchmaking`, ADR-035), but are otherwise independent deployables with
  their own container, own `docker-compose.yml` entry, and a gRPC contract
  (`proto/matchmakingv1`, not a shared database or shared in-process types)
  as the only link between them.

### Component responsibilities

| Component | Owns | Does NOT do |
|---|---|---|
| **matchmaking-service** (`cmd/matchmaking-service`, `internal/mmsvc`) | `POST /matchmaking/queue` (mint `MatchmakingClaims` + `ZADD NX`), `GET /matchmaking/stream` (SSE), `GET /matchmaking/status` (polling backstop), `DELETE /matchmaking/queue` (cancel), queue-timeout sweep (`Sweep`, own 5s ticker), `MatchReportService` gRPC server (`ReportServer`) | Never picks or assigns a chess-server instance. Never touches `GameStore`/`GameRegistry`/Redis ownership keys/Postgres at all. Never decides who plays whom. |
| **chess-server (`internal/matchmaking`)** | Per-instance pairing loop (`ZPOPMIN queue 2`), `Manager.CreateMatchedGame` (atomic two-player insert, `GameRegistry` registration, `ClaimOwnership`, arms `matchedOpponentConnectTimeout`, mints both `MatchedGameTokens` pairs), re-enqueue-on-failure, gRPC client reporting outcomes, `matchmaking_active_game:{userID}` writes (folded into the existing heartbeat tick in `internal/game/heartbeat.go`) | Never talks to another chess-server instance. Never holds or knows about an SSE connection. |

### Database columns (implemented — migrations 005/006)

```sql
-- migration 005 (ADR-034):
ALTER TABLE games ADD COLUMN matchmaking_request_id UUID UNIQUE;
-- Nullable: only matched games populate it. UUID v4 (correlation/idempotency
-- token generated per pairing attempt, reused across retries of that same
-- attempt) — NOT the same convention as games.id, which is UUID v7.
-- Insert issued with ON CONFLICT (matchmaking_request_id) DO NOTHING,
-- making CreateMatchedGame safe to retry under ACK loss.

-- migration 006 (ADR-041/042):
ALTER TABLE games ADD COLUMN activated_at TIMESTAMPTZ;
-- NULL until WAITING_FOR_PLAYER → ACTIVE, set atomically with that
-- transition (GameStore.ActivateGame). Anchors both ADR-041's first-move
-- grace period and ADR-042's matched-opponent-never-connected timeout.
```

See the Database Schema section above for the full current `games`/`users`
table definitions, including migration 007's `username`/`password_hash`
(ADR-043).

### Redis keys (implemented)

```
matchmaking:queue:10+0                    ZSET, member=userID, score=enqueue timestamp
matchmaking_active_game:{userID}          STRING (JSON: gameID, playerToken, instanceLabel,
                                           wsPath), TTL=OwnershipTTL (30s), renewed every
                                           OwnershipRenewInterval (10s) by chess-server's
                                           existing heartbeat loop — written only by
                                           chess-server, read-only for matchmaking-service.
                                           playerToken (renamed from connectToken, ADR-044 —
                                           see Match Notification below) is a long-lived
                                           PlayerClaims, deliberately safe to leave unrenewed
                                           between heartbeat ticks.
matchmaking_result:{userID}               STRING (JSON), TTL=5min. Written by ReportServer
                                           (on match/failure) or Sweep (on queue timeout);
                                           read by GET /matchmaking/status. The lost-RPC/
                                           lost-SSE-push backstop.
reported:{gameID}                         STRING ("1"), TTL=5min. ReportMatchCreated's own
                                           idempotency key — covers a chess-server gRPC retry
                                           after a lost ACK without double-delivering MATCH_FOUND.
```

### gRPC contract (implemented)

Unary, two methods, shared-secret interceptor for the trust boundary (Docker
Compose has no `NetworkPolicy` equivalent — deliberate interim choice,
mTLS post-Phase-3). Full `.proto` and reasoning: `DECISIONS_LOG_PHASE_3.md`
ADR-033, ADR-044; `phases/current/PHASE_3_DESIGN_NOTES.md` §6, §18.

```
service MatchReportService {
  rpc ReportMatchCreated(MatchCreatedRequest) returns (MatchCreatedResponse);
  rpc ReportMatchmakingFailed(MatchmakingFailedRequest) returns (MatchmakingFailedResponse);
}

message MatchCreatedRequest {
  string game_id             = 1;
  string white_user_id       = 2;
  string black_user_id       = 3;
  string white_player_token  = 4;  // signed PlayerClaims — 24h, fallback for a later /resolve
  string black_player_token  = 5;
  string white_connect_token = 7;  // signed ConnectClaims — short-lived, directly dialable (ADR-044)
  string black_connect_token = 8;
  string instance_label      = 6;
}
```

### Match notification: SSE, two connect paths (ADR-044)

Match notification does **not** reuse the WebSocket infrastructure
(`internal/ws`) at all — it's plain HTTP `text/event-stream` served by
matchmaking-service, authenticated by `MatchmakingClaims{UserID}` (same
signing secret as `PlayerClaims`/`ConnectClaims`, distinguished by claim
shape).

```
event: MATCH_FOUND
data: {"gameID":"...","connectToken":"...","playerToken":"...","instanceLabel":"...","wsPath":"/connect/..."}

event: MATCHMAKING_FAILED
data: {"reason":"RETRIES_EXHAUSTED"}   // or "QUEUE_TIMEOUT" (Sweep, generated natively — never
                                       // translated from the gRPC enum, which only ever carries
                                       // RETRIES_EXHAUSTED)
```

**Two distinct tokens, two distinct connect paths — this is the corrected
design, not the original one** (`DECISIONS_LOG_PHASE_3.md` ADR-044,
2026-08-17, superseding this section's original claim that `connectToken`
was a reused `PlayerClaims` requiring `/resolve` for every connection):

- **`connectToken`**: a genuinely fresh, short-lived `ConnectClaims`
  (carries `instanceLabel`), minted directly inside `CreateMatchedGame` at
  match time. Dial `/connect/{instanceLabel}` with it **immediately, no
  `/resolve` call** — safe specifically because the instance minting it is
  synchronously about to claim ownership of the new game, so there is zero
  elapsed time between minting and the instance actually being correct.
- **`playerToken`**: the player's ordinary long-lived `PlayerClaims`,
  fallback for any *later* reconnect. If `connectToken` is ever rejected as
  stale (SSE push missed, `/status` polled minutes later, connection
  dropped and reopened), the client falls back to
  `GET /games/{id}/resolve` with `Authorization: Bearer <playerToken>` —
  the exact same path an ordinary shared-link reconnect already uses, not a
  matchmaking-specific mechanism.

The same distinction applies to the active-game marker's 409 response
(`ALREADY_IN_ACTIVE_GAME`) and `GET /matchmaking/status`'s `"matched"`
shape — the marker's field is `playerToken` only (that read pattern is
genuinely unbounded-time-since-write, so `PlayerClaims` + forced `/resolve`
is correct there, unchanged from the original design); `"matched"` status
carries both tokens, same as `MATCH_FOUND`, since it's populated by the
same `ReportMatchCreated` write.

### Known gaps (as-built, not aspirational)

**CLOSED (2026-08-10, ADR-041/ADR-042):** the original version of this
section described an unsolved gap — a matched player who connects and
stays connected while their assigned opponent never connects at all had no
timer protecting them. `matchedOpponentConnectTimeout`, armed synchronously
in `CreateMatchedGame`, closes this. See the Game State Machine section
above for full detail, including the related first-move grace period
(ADR-041) this same work surfaced and closed.

**Open, found during Step 7's live E2E run (`scenario5`), not yet closed:**
when `CreateMatchedGame` fails deterministically (e.g. a userID with no
corresponding `users` row — an FK violation, not a transient error),
`PairingLoop.reenqueue` puts both players back at their original queue
scores (ADR-034) — but their underlying problem is permanent, so the very
next pairing-loop tick (500ms later) immediately re-picks them and fails
again, in an unbounded retry loop with no backoff. There is currently no
distinction anywhere in this system between a transient `CreateMatchedGame`
failure (worth retrying) and a deterministic one (never will succeed,
should stop retrying and surface differently). Not solved here — flagged
for its own future ADR, same treatment the original opponent-never-connects
gap received before ADR-041/042 closed it.

---

## What Is Intentionally Not In This Architecture

- **No ORM**: Raw SQL via pgx/v5. ORMs hide query behavior and generate inefficient queries. You must know what your queries are.
- **No global state**: Everything is injected. No `var db *pgxpool.Pool` at package level.
- **No client-side game authority**: The client is a display terminal. It validates moves for UX only.
- **No ELO-based or skill-based matching**: Phase 3's queue is FIFO by wait time only. ELO doesn't exist yet (Phase 4).
- **No general account/session system**: `POST /users`/`POST /login` (Phase 3, ADR-043) let a client obtain or recover a stable `userID` via username+password — that is the full extent of it. No issued token, no session, no expiration, no authorization checks anywhere else in the system: every endpoint still accepts a client-supplied `userID` at face value, exactly as before this addition. A real session/auth layer remains explicitly deferred, not implemented.
- **No cross-instance state synchronization**: Phase 2 deliberately routes to a single owning instance instead — there is never a second live `GameSession` for one game to synchronize with (`DECISIONS_LOG_PHASE_2.md` ADR-021). If a future phase needs genuine multi-writer state, that is a different, harder problem this project has not attempted.
