## Session Summary — Step 13 Wiring, E2E Bug Fixes

### What Was Built

- **`internal/api/game_handler_test.go`** (new) — 12 integration tests closing the Step 12 gap flagged at session start: `CreateGame` (success + token-claims verification, invalid UUID → 400, malformed JSON → 400), `JoinGame` (success, game-not-found → 404, already-joined → 409, self-play → 409, invalid UUID → 400), `GetGame` (success, not-found → 404), and `Health` (asserts the response body has **no** top-level `"data"` key, not just `status == "ok"`, so a future regression to the enveloped format would actually be caught). Reuses the real unexported response types (`createGameResponseData`, `errorDetail`, etc.) rather than parallel test structs, and drives setup through the HTTP layer itself rather than calling `Manager` directly.
- **`cmd/server/main.go`** (new) — full Step 13 wiring: env-based `config` with fail-fast on missing `DATABASE_URL`/`JWT_SECRET`, `store.NewPool`, `runMigrations` via golang-migrate's `pgx5://`-scheme driver (scheme verified directly against the driver's `init()` source, not assumed), full dependency graph construction in ARCHITECTURE.md's stated order, `RestoreActiveGames`, the ADR-018 server-lifetime context, `api.NewRouter`, and a 5-step `shutdown()` sequence (HTTP drain → cancel WS context → `wsRegistry.CloseAll()` → `PersistActiveClockState` → pool close).
- **`cmd/server/main_test.go`** (new) — unit tests for `loadConfig` (missing `DATABASE_URL`/`JWT_SECRET`, defaults for optional vars, explicit pass-through) and table-driven `parseLogLevel`.
- **`internal/game/manager.go`** — `HandleDisconnect` now persists paused clock state to the DB (previously in-memory only — a real gap against PHASE_1.md acceptance criterion #3, found by direct code review before Step 13 was written). New `Manager.PersistActiveClockState(ctx)` for the shutdown-time defense-in-depth flush. Both later revised (see Problems Encountered) to fix a shutdown-time context-cancellation bug caught by real E2E testing.
- **`internal/game/manager_test.go`** — `TestManager_HandleDisconnect_PersistsClockState` (regression test using a distinctive "stale sentinel" DB seed so it's deterministic without `time.Sleep`) and `TestManager_HandleDisconnect_ClockNotStarted_NoOp`.
- **`internal/game/session.go`** — new `GameSession.CloseConnections(statusCode int, reason string)`, sending a close frame to both players and clearing their connection slots.
- **`internal/game/messages.go`** — new `wsCloseNormal = 1000` constant (RFC 6455 normal closure), kept as a bare int rather than importing `gorilla/websocket` into `internal/game`.
- **`internal/api/ws_handler.go`** — one-line fix threading `h.ctx` into the now-context-taking `HandleDisconnect` call.
- **`internal/api/ws_handler_test.go`** — new `TestWSHandler_GameOver_ClosesConnectionsAfterDelivery`, using real dialed WebSocket connections (not fakes) to prove both that connections close after game-over **and** that `GAME_OVER` is strictly delivered before the close frame — the second assertion is what actually exercises the ordering fix, not just "eventually closed."
- **`Makefile`** — `test-integration` target updated to run with `-p 1` (done by user, confirmed working; not re-touched this session).

### Decisions Made

- **`GET /health`'s flat (non-enveloped) response — explicitly confirmed this session**, resolving the item that was previously "pending sign-off." Rationale: health checks are consumed by infrastructure that expects a trivial top-level shape, not application clients; PHASE_1.md's own spec example is flat; kept as a narrow, documented exception to CODING_GUIDELINES §7, not precedent for anything else.
- **Migrations run automatically on `main.go` startup — kept, not reverted.** Flagged as **TD-008** (below) rather than an ADR, since there's no competing design being chosen between right now — just an accepted Phase 1 simplification with an explicit revisit trigger (Phase 2 multi-instance deployment, where this becomes a real lock-contention and privilege-separation problem).
- **Two decisions from this session should be formally logged as ADRs next session — flagging now, not yet written into `DECISIONS_LOG_PHASE_1.md`:**
  1. **Detached-context pattern for `HandleDisconnect`'s clock-persist write.** Uses `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)` instead of the caller's context directly, because ADR-018's own cancellation ordering (cancel server-lifetime context *before* `ws.Registry.CloseAll()`) guarantees that context is already cancelled by the time `CloseAll`-triggered disconnects reach this code — the persist would fail on **every** graceful shutdown, not as a rare race. This is a genuine architectural decision (reorder shutdown vs. detach the context) with real tradeoffs, same category as ADR-014/016/017.
  2. **`GameSession.CloseConnections` must only be called from the same goroutine, immediately after, whichever call sent the terminal `GAME_OVER` message** (`startEventSubscriber`'s terminal branch, `publishGameOver`'s fallback) — never from `Manager.finalizeGame`, which runs on a different goroutine and would race the close frame against `GAME_OVER`'s own delivery through the shared per-connection outbound queue. Confirmed via direct reading of `eventbus.go`: `LocalEventBus.Publish`'s buffered-channel send only guarantees the event was *enqueued*, not that the subscriber goroutine has processed it yet.

### Tradeoffs Considered

- **Reordering `main.go`'s shutdown steps** (`CloseAll()` before `cancelWSCtx()`) instead of detaching the persist context — rejected because it would reopen the exact race ADR-018 was written to prevent on the `HandleMessage` side (in-flight move processing continuing after connections are force-closed).
- **Closing connections from `finalizeGame`** (one centralized call site, simpler) vs. from the terminal-broadcast goroutine (correct ordering, two call sites) — chose correctness over centralization once the buffered-channel semantics were confirmed; a doc-comment warning was added to `finalizeGame` specifically to prevent a future session from "simplifying" this back into the race.
- **Testing the close-after-game-over fix at the `internal/game` package level** (constructing fake `*ws.Connection`s) vs. `internal/api` with real dialed WebSocket connections through the actual `WSHandler` — chose the latter, since the bug is fundamentally about wire-level byte ordering, which fake connections can't demonstrate.

### Lessons Learned

- A context's cancellation timing, chosen correctly for one operation (ADR-018's `HandleMessage` safety), can silently poison a *different* operation (the new clock-persist write) once both are wired through the same shared context — adding new I/O to an existing ctx-taking function requires re-examining the caller's cancellation lifecycle, not just adding the parameter mechanically.
- A buffered channel's successful send (`Publish` returning `nil`) does not mean the consumer has processed the value yet — assuming synchronous-like ordering from an actually-async primitive nearly produced a second production bug (closing connections from the wrong goroutine) before `eventbus.go` was checked directly.
- Manual E2E testing (real process, real `Ctrl+C`, real `wscat` terminals) caught two real bugs that the full `-race -tags integration` suite and gopls diagnostics missed entirely — validates PHASE_1.md's decision to make Step 14 a separate, mandatory, non-automated checklist rather than folding it into Step 1–13's test coverage.

### Problems Encountered

- **Graceful shutdown logged `"context canceled"` on every clock-persist attempt** — not a rare race, 100% reproducible, caught directly from the user's terminal output. Root cause and fix described above.
- **WebSocket connections never closed after game-over** — user observed this directly via resignation testing (`wscat` stayed open, further messages produced silent non-responses). `finalizeGame` never touched connections at all; fixed via `CloseConnections`, placed carefully to avoid a second, more subtle bug (message-ordering race against `GAME_OVER`).
- **Self-inflicted, caught before handoff:** while drafting `manager_test.go`'s new tests, introduced a broken comment (missing `//` prefix on a continuation line) and a leftover dead-code fragment. Caught by immediate re-read before the user built anything — not by a failed build.

### Checklist Progress

- ✅ `game_handler_test.go` — Step 12 test gap fully closed
- ✅ Step 13 (`cmd/server/main.go`) — implemented; `go build`/`go vet`/`go test -race`/`go test -race -tags integration -p 1` all confirmed passing
- ✅ `cmd/server/main_test.go` — `loadConfig`/`parseLogLevel` unit coverage
- ✅ `TestManager_HandleDisconnect_PersistsClockState` / `_ClockNotStarted_NoOp` — added, confirmed passing
- ✅ PHASE_1.md Step 14 (E2E manual verification) — health check, create/join, move pipeline, turn/illegal-move rejection, reconnection, and kill-9 restart all exercised successfully; graceful shutdown and resignation both surfaced real bugs, now fixed
- 🔄 TD-008 — documented, correctly deferred to pre-Phase-2 work, not yet resolved

### Technical Debt Introduced

**TD-008**: Migrations run automatically on server startup (`cmd/server/main.go`'s `runMigrations`, called unconditionally before the dependency graph is constructed). Acceptable for Phase 1's single-instance deployment (ROADMAP.md explicitly defers the multi-instance problem to Phase 2). Must be revisited before Phase 2 ships multiple concurrent instances — they will contend on golang-migrate's Postgres advisory lock during startup, coupling pod-readiness time to lock contention, and an app process with DDL privileges widens blast radius unnecessarily. Migration execution should likely move to a separate deploy step (CI job, `make migrate-up` as a pre-deploy gate) decoupled from application boot. | Phase introduced: 1 (Step 13) | Must fix by: Phase 2

No other new technical debt — the two bugs fixed this session were correctness fixes closing real gaps, not shortcuts trading correctness for speed.

### Files Modified

**Created:**
- `internal/api/game_handler_test.go`
- `cmd/server/main.go`
- `cmd/server/main_test.go`

**Modified:**
- `internal/game/manager.go` — `HandleDisconnect` ctx signature + clock persist + detached-context fix; new `PersistActiveClockState`; `publishGameOver` fallback close; `startEventSubscriber` close-after-`GAME_OVER`; `finalizeGame` doc comment warning
- `internal/game/session.go` — new `CloseConnections` method
- `internal/game/messages.go` — new `wsCloseNormal` constant
- `internal/game/manager_test.go` — two new `HandleDisconnect` tests
- `internal/api/ws_handler.go` — pass `h.ctx` into `HandleDisconnect`
- `internal/api/ws_handler_test.go` — new `TestWSHandler_GameOver_ClosesConnectionsAfterDelivery` + `assertConnectionClosedNormally` helper
- `Makefile` — `test-integration` target uses `-p 1` (done by user)

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
**Status: ✅ Completed Step 14 (End-to-End Verification): core flows verified, two real bugs found via manual E2E testing and fixed (shutdown clock-persist context cancellation; WebSocket connections not closing after GAME_OVER).**

---

## Completed Work

### Documentation
- [x] Project purpose and scope defined
- [x] Full tech stack decided and rationale documented
- [x] All documentation files created
- [x] Phase 1 spec written (PHASE_1.md)
- [x] Architecture documented (ARCHITECTURE.md)
- [x] All ADRs logged (DECISIONS_LOG_PHASE_1.md) — through **ADR-018**; **two more flagged this session, not yet formally logged** (see Pending ADRs below)
- [x] Handler package relocation corrected (`internal/ws/handler.go` → `internal/api/ws_handler.go`) across all three docs
- [x] `GET /health`'s flat response shape — **explicit sign-off received this session**, no longer a pending item

### Implementation
- [x] WebSocket infrastructure (`internal/ws`): connection lifecycle, read loop, write loop, heartbeats, registry, graceful shutdown
- [x] Step 1: Project Scaffold
- [x] Step 2: Database Migrations
- [x] Step 3: Store Layer
- [x] Step 4: Auth Layer
- [x] Step 5: Chess Layer
- [x] Step 6: Game Session and Registry
- [x] Step 7: EventBus
- [x] Step 8: Move Pipeline
- [x] Step 9: Clock
- [x] Step 10: Manager
- [x] ADR-014 (TOCTOU re-check), ADR-015 (abandonment semantics), ADR-016 (JoinGame race), ADR-017 (HandleConnect atomicity + `transitionLocked`), ADR-018 (ReadLoop context lifetime) — all implemented and tested in prior sessions
- [x] Step 11: WebSocket Handler (`internal/api/ws_handler.go`)
- [x] Step 12: HTTP API Handlers (`internal/api/game_handler.go`, `response.go`, `routes.go`) — **`game_handler_test.go` written this session, closing the previously-flagged test gap: `CreateGame`/`JoinGame`/`GetGame`/`Health` now each have dedicated integration tests, not just implicit coverage via `ws_handler_test.go`'s wiring**
- [x] **Step 13: Main and Wiring (`cmd/server/main.go`, this session)** — config loading (fail-fast on missing `DATABASE_URL`/`JWT_SECRET`), `store.NewPool`, automatic migrations on startup via golang-migrate's `pgx5://`-scheme driver (verified against source, see TD-008 for the tradeoff this introduces), full dependency graph construction, `RestoreActiveGames`, ADR-018's server-lifetime context, `api.NewRouter`, HTTP server start, signal handling, and a 5-step graceful `shutdown()` sequence
- [x] **`cmd/server/main_test.go` (this session)** — unit tests for `loadConfig` and `parseLogLevel`
- [x] **`Manager.HandleDisconnect` clock-persist fix (this session)** — previously only paused the clock in memory; now persists the paused reading to the database, closing a real gap against PHASE_1.md acceptance criterion #3 (server restart correctness). Required a signature change (`ctx context.Context` added as first argument, since the function now performs I/O) and, after E2E testing surfaced a shutdown-time bug, a further revision to use a detached context (see Known Sharp Edges)
- [x] **`Manager.PersistActiveClockState` (this session)** — shutdown-time defense-in-depth clock flush, called from `main.go`'s `shutdown()`
- [x] **`GameSession.CloseConnections` (this session)** — closes both players' WebSocket connections after a game ends. Found necessary via manual E2E testing (resignation left sockets open indefinitely); placement was non-trivial — must be called from the same goroutine as, and immediately after, whichever call sent the terminal `GAME_OVER` message, never from `finalizeGame` (see Known Sharp Edges for why)
- [x] **`TestWSHandler_GameOver_ClosesConnectionsAfterDelivery` (this session)** — regression test using real dialed WebSocket connections proving both that connections close after game-over and that `GAME_OVER` is delivered strictly before the close frame
---

## Pending ADRs (flagged this session, not yet formally logged in DECISIONS_LOG_PHASE_1.md)

1. **Detached-context pattern for cleanup writes driven by a cancellable server-lifetime context.** `HandleDisconnect`'s clock-persist DB write uses `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)` rather than the caller's `ctx` directly, because ADR-018's shutdown ordering (cancel context *before* `ws.Registry.CloseAll()`) guarantees that `ctx` is already cancelled by the time every `CloseAll`-triggered disconnect reaches this code. Without this, the persist fails on **100% of graceful shutdowns**, not as an edge case — confirmed via real E2E testing this session.
2. **WebSocket connection closure after a terminal game event must happen same-goroutine, immediately after the `GAME_OVER` broadcast — never from `Manager.finalizeGame`.** `finalizeGame` runs on a different goroutine than whichever goroutine actually sent `GAME_OVER` (the `EventBus` subscriber, or `publishGameOver`'s fallback path), and `LocalEventBus.Publish`'s buffered-channel send only guarantees the event was enqueued for that goroutine to eventually process — not that it already has. Closing from `finalizeGame` would race the close frame against `GAME_OVER`'s own delivery through the same per-connection outbound queue.

Both should be written up as full ADR entries next session before being considered settled.

---

## Phase 1 Checklist

### Foundation
- [x] go.mod initialized with all dependencies
- [x] .env.example created
- [x] docker-compose.yml created (PostgreSQL + Redis placeholder)
- [x] Makefile created with standard targets — **`test-integration` target updated this session to run with `-p 1`** (see Known Sharp Edges)
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
- [x] GameState machine (WAITING → ACTIVE → COMPLETED / ABANDONED) — corrected transition semantics per ADR-015
- [x] GameRegistry (gameID → *GameSession)
- [x] EventBus interface defined (LocalEventBus for Phase 1)
- [x] Player-to-connection bridge for reconnection (session pointer slots + ReplaceConnection)
- [x] `GameSession.IsPlayerConnected(color)`
- [x] messages.go — single source of truth for all WebSocket protocol strings, **now also holds `wsCloseNormal` (this session)**
- [x] MoveProcessor — full 8-step pipeline, clock-switching, TOCTOU re-check (ADR-014)
- [x] MoveRejectionError
- [x] Clock — per-game countdown timers, timeout detection, channel-based goroutine
- [x] Manager — top-level orchestrator, `finalizeGame` centralized registry cleanup
- [x] `GameSession.RegisterConnection` atomic compound operation (ADR-017)
- [x] `GameSession.transitionLocked`
- [x] `Manager.GetGame` thin passthrough
- [x] **`Manager.HandleDisconnect` clock persistence (this session)** — ctx-taking signature, detached-context write
- [x] **`Manager.PersistActiveClockState` (this session)**
- [x] **`GameSession.CloseConnections` (this session)**

### API Layer (HTTP)
- [x] chi router setup
- [x] POST /games
- [x] POST /games/:id/join
- [x] GET /games/:id
- [x] GET /health — **flat response shape explicitly confirmed this session, no longer a pending decision**

### WebSocket Layer
- [x] ws infrastructure ported and updated
- [x] `ws.Connection.Start`
- [x] `ws.Connection.Send`/`SendCloseFrame`/`enqueuePing` select-race bug fixed
- [x] WS upgrade handler at GET /ws/game/:id
- [x] Token validation on connect
- [x] Player registration into GameSession on connect
- [x] Message routing (MOVE, RESIGN, PING)
- [x] **`ws_handler.go` passes `h.ctx` into `HandleDisconnect` (this session)**

### Move Pipeline
- [x] Full 8-step pipeline, TOCTOU re-check, MoveRejectionError typed errors — unchanged this session

### Time Controls
- [x] Server-side clock per game
- [x] Clock starts when both players connected
- [x] Clock switches on each move
- [x] Timeout detection goroutine per game
- [x] GAME_OVER broadcast on timeout
- [x] **Clock state persisted on disconnect, not just paused in memory (this session)**
- [x] **Clock state flushed defensively on graceful shutdown (this session)**

### Reconnection
- [x] All previously-implemented reconnection behavior unchanged this session

### Abandonment (ADR-015 correction)
- [x] Unchanged this session

### Persistence Recovery
- [x] Unchanged this session

### Server Wiring (Step 13, this session)
- [x] Config loaded from environment with fail-fast on required vars
- [x] pgxpool initialized
- [x] Migrations run automatically on startup (see TD-008 for the accepted tradeoff)
- [x] Full dependency graph constructed in ARCHITECTURE.md order
- [x] `RestoreActiveGames` called before serving traffic
- [x] Routes registered, HTTP server started
- [x] SIGTERM/SIGINT handled with a 5-step graceful shutdown sequence, bounded by a 15s timeout
- [x] §14 manual E2E verification

### Testing
- [x] Store layer: integration tests (real PostgreSQL)
- [x] Auth layer: unit tests
- [x] Chess layer: unit tests
- [x] Game session and registry: unit tests
- [x] EventBus: unit tests
- [x] Move pipeline: integration tests
- [x] Clock: unit tests with goleak
- [x] Manager: integration tests, concurrency regression tests
- [x] WebSocket handler: httptest-based integration tests
- [x] **`game_handler_test.go` (this session)** — closes the previously-flagged gap; `CreateGame`/`JoinGame`/`GetGame`/`Health` all have dedicated HTTP-layer tests now, driven through the actual handlers, not just implicit Manager-call coverage
- [x] **`cmd/server/main_test.go` (this session)** — `loadConfig`/`parseLogLevel` unit coverage
- [x] **`TestManager_HandleDisconnect_PersistsClockState` / `_ClockNotStarted_NoOp` (this session)**
- [x] **`TestWSHandler_GameOver_ClosesConnectionsAfterDelivery` (this session)**

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
| ADR-013 | Chess move validation strategy | Validate-Then-Apply split |
| ADR-014 | ProcessMove TOCTOU fix | Re-check session status under `session.mu.Lock()` before ApplyMove |
| ADR-015 | Abandonment semantics correction | Single-disconnect → COMPLETED, opponent wins. Both-disconnected → ABANDONED, DRAW |
| ADR-016 | JoinGame double-join race | Atomic conditional UPDATE, `store.ErrGameNotJoinable` sentinel |
| ADR-017 | HandleConnect concurrent first-connect race | `RegisterConnection` atomic compound operation |
| ADR-018 | ReadLoop / HandleMessage context lifetime | Server-lifetime `context.Context`, cancelled on SIGTERM |
| *pending* | Detached-context pattern for HandleDisconnect's clock persist | Flagged this session — see Pending ADRs above, not yet formally logged |
| *pending* | CloseConnections must be same-goroutine, immediately-after GAME_OVER send | Flagged this session — see Pending ADRs above, not yet formally logged |

### Implementation Decisions (No ADR Required)

| Decision | Chosen | Rationale |
|----------|--------|-----------|
| `UpdateGameStatus` signature | `*GameOutcome` (not `*Outcome`) | outcome + reason always move together |
| Game UUID generation | App-generated (game layer), not DB DEFAULT | JWT must be signed before DB round-trip |
| Game ID UUID version | UUID v7 via `github.com/google/uuid` | Time-ordered IDs avoid B-tree index fragmentation |
| `scanGame` helper | `func(dest ...any) error` parameter | Both `pgx.Row.Scan` and `pgx.Rows.Scan` satisfy this |
| Nullable column scanning | `*string` intermediates | Avoids pgx/v5 reflection path |
| Store test package | `package store` (internal) | Too many exported domain types to prefix |
| `Color` in PlayerClaims | `string` (not `store.Color`) | Keeps `internal/auth` free of `internal/store` dependency |
| `GetMovesForGame`/`GetActiveGames` empty result | `make([]*T, 0)` (non-nil) | Serializes to `[]` not `null` in JSON |
| `MoveHistory` uses `g.Moves()` + `g.Positions()` | Not `g.MoveHistory()` | Library bug: panics on nil comments slice |
| `DetectOutcome` default draw reason | `"DRAW_AGREEMENT"` | Only valid schema value for non-stalemate draws |
| `RestoreActiveGames` board reconstruction | Always `GameFromMoves`, never `GameFromFEN` | current_fen can be stale (TD-007) |
| `LocalEventBus.Publish` holds RLock during sends | Snapshot-then-release rejected | Send-on-closed panic risk |
| `MoveRejectionError` typed error | Manager uses `errors.As` | Distinguishes client rejections from infrastructure failures |
| `GameSession.RegisterConnection` returns `(activated bool, err error)` (ADR-017) | Atomic compound operation | Closes concurrent first-connect race |
| `transitionLocked(newStatus) error` | Unexported, lock-free, shared by `Transition()`/`RegisterConnection` | Single source of truth for legal edges |
| `ws.Connection.Start(onMessage, onClose)` | Exported bundling method | `wg` intentionally unexported |
| `Send`/`SendCloseFrame`/`enqueuePing` two-stage `select` | Not one combined three-case select | Non-deterministic once queue has room |
| `WSHandler` located in `internal/api`, not `internal/ws` | Circular-import correction | `internal/game` already imports `internal/ws` |
| `GameHandler` depends on `*store.UserStore` directly | Deviates from PHASE_1.md's literal spec | Manager's Create/JoinGame assume the user row already exists |
| `Manager.GetGame` passthrough | Thin wrapper | Keeps `internal/api`'s only game-layer dependency as `game.Manager` |
| **`GET /health` flat, non-enveloped response** | **Confirmed this session** | Infrastructure health-probe convention; matches PHASE_1.md's own literal example |
| **Migrations run automatically in `main.go` on startup (this session)** | **Kept, logged as TD-008** | Correct for Phase 1's single-instance deployment per ROADMAP.md; must be revisited before Phase 2 |
| **`HandleDisconnect(ctx, gameID, color)` signature change (this session)** | ctx added as first argument | Function now performs I/O (clock persist); CODING_GUIDELINES §2 applies |
| **`HandleDisconnect`'s clock-persist write uses `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)` (this session)** | Detached from parent cancellation, still bounded | ADR-018's shutdown ordering guarantees the parent ctx is already cancelled by the time this code runs during `CloseAll` — see Pending ADRs |
| **`GameSession.CloseConnections` called only from the terminal-broadcast goroutine (this session)** | Never from `finalizeGame` | Avoids a message-ordering race against `GAME_OVER`'s own delivery — see Pending ADRs |
| **`wsCloseNormal` defined as a bare `int` in `internal/game/messages.go` (this session)** | Not a `gorilla/websocket` import into `internal/game` | Preserves the internal/ws / internal/game protocol-detail boundary |
| **`Makefile`'s `test-integration` target runs with `-p 1` (this session, user-applied)** | Forces package-level sequencing | `go test ./...` runs package binaries concurrently by default; all integration tests share one real DB (`chess_dev`) with no isolation, so `truncateAll` in one package racing against inserts in another produced intermittent FK-violation failures |

---

## Technical Debt

```
TD-001: Player token passed in URL query parameter (visible in logs) | Phase 1 | Fix by: Phase 3
TD-002: Clock pauses on disconnect (disconnect-stalling possible) | Phase 1 | Fix by: Phase 4
        Note (this session): the persistence half of this gap — paused clock
        state never reaching the DB — is now fixed (HandleDisconnect persists
        on pause). The underlying "clock pauses instead of continuing to run"
        design choice itself remains open, tracked here as before.
TD-003: No draw offer mechanism | Phase 1 | Fix by: Phase 4
TD-004: Anonymous identity only (no real user accounts) | Phase 1 | Fix by: Phase 3
TD-005: Single time control (10+0 only) | Phase 1 | Fix by: Phase 4
TD-006: DetectOutcome maps ThreefoldRepetition/FiftyMoveRule/InsufficientMaterial to "DRAW_AGREEMENT" | Phase 1 | Fix by: Phase 4
TD-007: GameFromFEN loses position history — threefold repetition blind after server restart | Phase 1 | Fix by: Phase 4
TD-008: Migrations run automatically on server startup (cmd/server/main.go's runMigrations, unconditional,
        before the dependency graph is built) | Phase 1 (Step 13) | Fix by: Phase 2
        Acceptable now: Phase 1 is explicitly single-instance (ROADMAP.md defers the scaling
        problem to Phase 2 on purpose). Must be revisited before Phase 2 ships multiple
        concurrent instances: they will contend on golang-migrate's Postgres advisory lock
        during startup, coupling pod-readiness time to lock contention, and an app process
        with DDL privileges widens blast radius unnecessarily beyond what runtime DML needs.
        Likely fix: move migration execution to a separate deploy step (CI job, or
        `make migrate-up` as a pre-deploy gate) decoupled from application boot.
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
7. **Every I/O function takes context.Context as its first argument.**
8. **ValidateMove before DB write; ApplyMove after DB write succeeds.** (ADR-013)
9. **Check for an existing, well-tested library before writing custom logic for solved problems.**
10. **Any read-then-write sequence spanning two or more statements must be checked for concurrent-caller correctness, not just sequential correctness, before being considered complete.** (ADR-014, ADR-016, ADR-017) A test that only exercises sequential calls does not prove a fix; a dedicated concurrent-goroutines test against real infrastructure is required when the sequence guards a uniqueness or exclusivity invariant. **This session extends the same discipline to a related but distinct failure mode**: a *send-then-close* sequence across an async channel (EventBus) has the same "prove it, don't reason about it" requirement — a test using real dialed WebSocket connections was required to prove `GAME_OVER` is delivered before the close frame, not just that the connection eventually closes.
11. **`internal/api` is the only package permitted to import both `internal/ws` and `internal/game`.**
12. **A context's cancellation timing chosen for one operation must be re-verified for every other operation later wired through the same context — never assumed safe by inheritance.** (New this session, following the shutdown clock-persist bug.) When adding new I/O to a function that already takes a `context.Context` for an unrelated reason, explicitly check what cancels that context and when, relative to when the new code path runs.
13. **A message-delivery-then-connection-close sequence must happen in the same goroutine that performed the delivery, in program order — never split across goroutines relying on a queue/channel's "accepted" signal as a proxy for "processed."** (New this session, following the GAME_OVER-then-close bug.)

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://`. `cmd/server/main.go`'s `runMigrations` converts this to `pgx5://` before calling `migrate.New` — golang-migrate's pgx/v5 driver package registers itself under the `pgx5` scheme (verified directly against the driver's `init()` source this session, not assumed), internally rewriting back to `postgres://` before handing off to `database/sql` via its own blank-imported `pgx/v5/stdlib` adapter. `store.NewPool` still receives the original `postgres://`-form URL unchanged.

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Always use the `chess.MoveHistory(g)` wrapper.

- **`MoveHistory()` returns annotated SAN.** `+` for check, `#` for checkmate.

- **`DetectOutcome` must be called after `ApplyMove`.**

- **`ReadLoop`/`HandleMessage` context lifetime (ADR-018).** `WSHandler` holds a server-lifetime `context.Context`, injected at construction by `cmd/server/main.go`, cancelled on SIGTERM/SIGINT **before** `ws.Registry.CloseAll()`.

- **`Manager.HandleDisconnect`'s clock-persist write does NOT use the passed-in `ctx` directly (this session).** It wraps it in `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`. This is deliberate, not an oversight: during graceful shutdown, `ctx` (the ADR-018 server-lifetime context) is cancelled *before* `ws.Registry.CloseAll()` runs, and `CloseAll` is exactly what triggers this code path for every connected player. Using `ctx` directly means the persist fails with `"context canceled"` on every single graceful shutdown — confirmed by real E2E testing this session, not a theoretical concern. If you ever add more I/O to `HandleDisconnect`, check whether it needs the same treatment before assuming `ctx` is safe to use as-is.

- **`GameSession.CloseConnections` must only be called from the same goroutine as, and immediately after, whichever call sent the terminal `GAME_OVER` message.** The two correct call sites are `Manager.startEventSubscriber`'s `MsgTypeGameOver` branch and `Manager.publishGameOver`'s `EventBus`-failure fallback branch. **Never call it from `Manager.finalizeGame`** — that runs on a different goroutine, and `LocalEventBus.Publish`'s buffered-channel send only guarantees the event was *enqueued* for the subscriber to eventually process, not that it has been. Closing from `finalizeGame` races the close frame against `GAME_OVER`'s own delivery through the same per-connection outbound queue, with a real chance the client never receives `GAME_OVER` at all.

- **`go test ./...` and `go test -tags integration ./...` must be run with `-p 1` for the integration-tagged suites.** All integration tests across `internal/store`, `internal/game`, and `internal/api` share one real database (`chess_dev`) with no isolation between packages, and each package's `truncateAll(t)` unconditionally wipes shared tables. Running package test binaries concurrently (Go's default) causes intermittent foreign-key-violation failures when one package's `truncateAll` fires mid-test in another package. The `Makefile`'s `test-integration` target already does this; if you ever run `go test` directly rather than through `make`, remember the flag.

- **`LocalEventBus.Publish`'s buffered channel send does not mean the subscriber has processed the event yet** — it only means the event was successfully enqueued (or dropped if the buffer, size 8, was full). Do not write code downstream of a `Publish` call that assumes the subscriber's side effects (e.g. `SendToBothPlayers`) have already run by the time `Publish` returns.

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** Do not "optimise" to snapshot-then-release.

- **`ComputeFENAfterMove` must only be called after `ValidateMove` returned nil for the same `(g, san)` pair.**

- **`ProcessMove` re-checks `session.status == ACTIVE` under `session.mu.Lock()` immediately before `ApplyMove` (ADR-014).**

- **`onAbandonTimeout` branches on `session.IsPlayerConnected(opponentOf(color))` (ADR-015).**

- **`finalizeGame(gameID)` must be called exactly once per game-ending event.** Deliberately does NOT close WebSocket connections — see the `CloseConnections` sharp edge above; do not add that here.

- **`JoinGame`'s actual correctness guarantee lives in `GameStore.UpdatePlayerBlack`'s SQL, not the Go-level pre-flight check (ADR-016).**

- **`store.ErrGameNotJoinable` is distinct from `store.ErrGameNotFound`.**

- **`goleak.IgnoreTopFunction` in `clock_test.go` exempts exactly one symbol** — re-verify if the pgx dependency version changes.

- **`Clock` struct requires `startedAt time.Time`.**

- **`Resume(color Color)` restarts the timer for `c.active`, not `color`.**

- **`Clock.run()` never acquires `c.mu`.**

- **`RestoreActiveGames` must reconstruct boards via `chess.GameFromMoves`, never `chess.GameFromFEN(game.CurrentFEN)`.**

- **`RestoreActiveGames` must detect and correct zombie ACTIVE games.**

- **`gopls` does not index files under non-default build tags (e.g. `//go:build integration`).** Confirmed repeatedly this session across every new integration-tagged test file — `go_diagnostics` reports "no package metadata" for all of them. This is expected, not an error; verification of these files requires an actual `go test -tags integration` run. `main.go`, `manager.go`, `session.go`, `messages.go`, and `ws_handler.go` (no build tag) were successfully diagnosed by gopls this session with no issues found.

- **`handleTimeout` must never call any `Clock` method** — it's invoked from within the Clock's own background goroutine.

- **`GameSession.RegisterConnection` is the sole place the WAITING→ACTIVE transition can originate from a connection event (ADR-017).**

- **`ws.Connection.Send`/`SendCloseFrame`/`enqueuePing` each use two sequential `select` statements, not one combined three-case `select`.**

- **`ws.Connection.Start(onMessage, onClose)` is the only supported way to launch a connection's goroutines from outside package `ws`.**

- **`internal/api` is the only package that may import both `internal/ws` and `internal/game`.**

- **`GameHandler.GetGame`, `CreateGame`, `JoinGame` are now covered by dedicated `game_handler_test.go` tests (this session)** — the prior "real test gap" flagged in earlier sessions is resolved.

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

**Note on connection lifecycle after GAME_OVER (this session):** the server now proactively closes both players' WebSocket connections (RFC 6455 normal closure, code 1000) immediately after `GAME_OVER` is delivered. A client must not assume the connection stays open after receiving `GAME_OVER`.

**Note on `ABANDONED` outcome pairing (ADR-015):** unchanged.

**Note on the REST layer's JSON envelope:** unchanged; `GET /health`'s flat exception is now explicitly confirmed, not pending.

---

## Key Files and Their Responsibilities

```
cmd/server/main.go             — Wires all dependencies, runs migrations, starts HTTP server, handles OS
                                  signals, 5-step graceful shutdown (this session)
cmd/server/main_test.go        — loadConfig/parseLogLevel unit tests (this session)
internal/ws/connection.go      — Connection struct, Start, read/write loops, heartbeat, two-stage select fixes
internal/ws/registry.go        — connID -> *Connection map with RWMutex, CloseAll graceful shutdown
internal/ws/errors.go          — ErrConnectionClosed, ErrQueueFull
internal/game/errors.go        — ErrGameNotFound, ErrConnectionOccupied, ErrInvalidTransition,
                                  MoveRejectionError, ErrGameNotJoinable, ErrSelfPlay
internal/game/messages.go      — All WebSocket message type strings, rejection reasons, error codes,
                                  wsCloseNormal (this session)
internal/game/session.go       — GameSession struct, state machine, connection management, snapshots,
                                  clock field, NewGameSessionFromDB, IsPlayerConnected, RegisterConnection,
                                  transitionLocked, CloseConnections (this session)
internal/game/registry.go      — gameID -> *GameSession map with RWMutex
internal/game/eventbus.go      — EventBus interface + LocalEventBus (buffered chan, size 8 per subscriber —
                                  see Known Sharp Edges re: Publish's async guarantees)
internal/game/manager.go       — Top-level orchestrator: CreateGame, JoinGame, HandleConnect,
                                  HandleDisconnect (ctx-taking + clock persist, this session), HandleMessage,
                                  RestoreActiveGames, GetGame, PersistActiveClockState (this session),
                                  handleResign/handleTimeout/onAbandonTimeout, finalizeGame (doc-updated
                                  this session), publishGameOver (CloseConnections call added),
                                  startEventSubscriber (CloseConnections call added), abandonment timers
internal/game/manager_test.go       — Integration tests, including HandleDisconnect clock-persist regression
                                  tests (this session)
internal/game/manager_race_test.go  — Concurrency regression: JoinGame race, ADR-016
internal/game/session_test.go       — RegisterConnection concurrency regression test
internal/game/move.go          — MoveProcessor: 8-step pipeline, TOCTOU re-check, handleGameOver
internal/game/move_test.go     — Integration tests (unaffected by this session's changes — verified directly)
internal/game/clock.go         — Clock: channel-based per-game countdown timers
internal/game/clock_test.go    — Unit tests with goleak
internal/game/eventbus_test.go — LocalEventBus unit tests (unaffected by this session's changes — verified directly)
internal/chess/errors.go       — ErrIllegalMove, ErrInvalidFEN sentinels
internal/chess/types.go        — GameOutcome{Winner, Reason}
internal/chess/validator.go    — Validator, NewGame, GameFromFEN, GameFromMoves, ValidateMove, ApplyMove,
                                  ComputeFENAfterMove, DetectOutcome, CurrentFEN, MoveHistory
internal/store/errors.go       — ErrGameNotFound, ErrUserNotFound, ErrGameNotJoinable
internal/store/models.go       — Domain types
internal/store/postgres.go     — NewPool (accepts postgres:// or pgx5://-scheme URLs)
internal/store/game_store.go   — CreateGame, GetGame, UpdateGameStatus, UpdateCurrentFEN,
                                  UpdatePlayerBlack, GetActiveGames, UpdateClocks
internal/store/move_store.go   — SaveMove, GetMovesForGame
internal/store/user_store.go   — CreateOrGetUser, GetUser
internal/auth/token.go         — PlayerClaims, SignPlayerToken, VerifyPlayerToken
internal/api/response.go       — Shared dataEnvelope/errorEnvelope/writeData/writeError
internal/api/ws_handler.go     — WSHandler: WebSocket upgrade, token verify, Manager handoff.
                                  HandleDisconnect call now passes h.ctx (this session)
internal/api/testmain_test.go  — Shared integration TestMain/testPool/truncateAll/mustCreateUser
internal/api/ws_handler_test.go — Invalid-token/valid-token/reconnect tests, plus new
                                  TestWSHandler_GameOver_ClosesConnectionsAfterDelivery (this session,
                                  result not yet confirmed)
internal/api/game_handler.go   — GameHandler: CreateGame, JoinGame, GetGame, Health
internal/api/game_handler_test.go — Full HTTP-layer test coverage for all four handlers (this session,
                                  closes the previously-flagged test gap)
internal/api/routes.go         — NewRouter: chi wiring, slog request logging, panic recovery, all routes
migrations/                    — SQL migration files (golang-migrate format), run automatically by
                                  cmd/server/main.go on startup (TD-008)
```

---

## Chess Layer Key Patterns

(Unchanged this session — see prior content, all still accurate.)

**Validate-then-apply split (ADR-013) with FEN computation between, plus ADR-014's re-check:**
```go
if err := validator.ValidateMove(session.board, san); err != nil {
    return &MoveRejectionError{Reason: RejectReasonIllegalMove}
}

fenAfter, err := chess.ComputeFENAfterMove(session.board, san)

if err := moveStore.SaveMove(ctx, move); err != nil {
    return fmt.Errorf(...)
}

session.mu.Lock()
if session.status != store.GameStatusActive {
    session.mu.Unlock()
    return false, nil
}
applyErr := validator.ApplyMove(session.board, san)
session.mu.Unlock()

session.clock.Switch()
```

**Chess layer API split:**
```
Package-level: chess.NewGame(), chess.GameFromFEN, chess.GameFromMoves, chess.CurrentFEN,
               chess.MoveHistory, chess.ComputeFENAfterMove
Validator methods: ValidateMove, ApplyMove, DetectOutcome
```

---

## GameSession Key Patterns

**State machine:**
```go
WAITING_FOR_PLAYER → ACTIVE
ACTIVE             → COMPLETED
ACTIVE             → ABANDONED
```

**Connection lifecycle (ADR-017):**
```go
activated, err := session.RegisterConnection(color, conn)
session.ReplaceConnection(color, conn)   // reconnect
session.ClearConnection(color)           // disconnect
session.IsPlayerConnected(color)         // abandonment check
```

**Closing connections after a game ends (this session) — see Known Sharp Edges for the full rationale:**
```go
// CORRECT — same goroutine, immediately after the GAME_OVER send:
session.SendToBothPlayers(gameOverPayload)
session.CloseConnections(wsCloseNormal, "game ended")

// WRONG — never do this from finalizeGame or any other goroutine:
// finalizeGame(gameID) { ...; session.CloseConnections(...) }  // races GAME_OVER delivery
```

**Sending messages — always through session:**
```go
session.SendToPlayer(store.ColorWhite, msgBytes)
session.SendToBothPlayers(msgBytes)
```

**Constructing a session:**
```go
session := NewGameSession(gameID, whiteUserID)
session := NewGameSessionFromDB(game, board)
```

---

## Clock Key Patterns

```go
clock := game.NewClock(InitialTimeMs)
clock := game.NewClockWithTimes(whiteMs, blackMs)
clock.SetTimeoutCallback(func(timedOut store.Color) { ... })
clock.Start(store.ColorWhite)
clock.Switch()
clock.Pause()
clock.Resume(activeColor)
remaining := clock.TimeRemaining(store.ColorWhite)
```

**Clock persistence on disconnect (this session):**
```go
// Inside HandleDisconnect, after session.clock.Pause():
whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()
session.UpdateClocks(whiteMs, blackMs) // in-memory

persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
m.gameStore.UpdateClocks(persistCtx, gameID, whiteMs, blackMs) // DB — detached context, see Known Sharp Edges
cancel()
```

**Test leak verification:**
```go
goleak.VerifyNone(t,
    goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
)
```

---

## Manager Key Patterns

**Game ID generation:**
```go
gameUUID, err := uuid.NewV7()
gameID := gameUUID.String()
```

**`PersistActiveClockState` (this session) — called once from main.go's shutdown():**
```go
func (m *Manager) PersistActiveClockState(ctx context.Context) {
    for _, session := range m.registry.AllActive() {
        whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
        blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()
        m.gameStore.UpdateClocks(ctx, session.ID, whiteMs, blackMs)
    }
}
```

**`finalizeGame` — centralized registry cleanup, does NOT touch connections (this session's doc clarification):**
```go
func (m *Manager) finalizeGame(gameID string) {
    m.cancelAbandonTimer(gameID, store.ColorWhite)
    m.cancelAbandonTimer(gameID, store.ColorBlack)
    m.registry.Unregister(gameID)
    // Deliberately no connection closing here — see Known Sharp Edges.
}
```

**`JoinGame`, `RestoreActiveGames`:** unchanged this session.

---

## EventBus Key Patterns

```go
payload, _ := json.Marshal(moveAppliedMsg{...})
bus.Publish(ctx, game.GameEvent{GameID: session.ID, Type: game.MsgTypeMoveApplied, Payload: payload})

ch, unsubscribe, err := bus.Subscribe(ctx, gameID)
m.startEventSubscriber(session, ch, unsubscribe)
```

**`startEventSubscriber`'s GAME_OVER branch now also closes connections, same goroutine (this session):**
```go
func (m *Manager) startEventSubscriber(session *GameSession, ch <-chan GameEvent, unsubscribe func()) {
    go func() {
        defer unsubscribe()
        for event := range ch {
            session.SendToBothPlayers(event.Payload)
            if event.Type == MsgTypeGameOver {
                session.CloseConnections(wsCloseNormal, "game ended")
                return
            }
        }
    }()
}
```

**DO NOT optimise `LocalEventBus.Publish` to snapshot-then-send.** **DO NOT assume `Publish` returning nil means the subscriber has processed the event** — see Known Sharp Edges.

---

## WebSocket Layer Key Patterns

**`Connection.Start`, `Send`/`SendCloseFrame`/`enqueuePing` two-stage select:** unchanged this session — see prior content.

---

## API Layer Key Patterns

**JSON envelope:** unchanged. `GET /health`'s flat exception is now explicitly confirmed sign-off, not pending.

**`WSHandler.ServeHTTP`'s onClose callback now passes h.ctx (this session):**
```go
conn.Start(
    func(raw []byte) { manager.HandleMessage(ctx, gameID, color, raw) },
    func() {
        manager.HandleDisconnect(h.ctx, gameID, color) // was: manager.HandleDisconnect(gameID, color)
        wsRegistry.Unregister(connID)
    },
)
```

---

## Server Wiring Key Patterns (Step 13, this session)

**Dependency graph construction order (main.go):**
```go
pool, _ := store.NewPool(ctx, cfg.DatabaseURL)
runMigrations(cfg.DatabaseURL) // pgx5://-scheme, see Known Sharp Edges

gameStore := store.NewGameStore(pool)
moveStore := store.NewMoveStore(pool)
userStore := store.NewUserStore(pool)

validator := internalchess.NewValidator()
eventBus := game.NewLocalEventBus()
processor := game.NewMoveProcessor(validator, gameStore, moveStore, eventBus)
registry := game.NewGameRegistry()
manager := game.NewManager(registry, processor, gameStore, moveStore, eventBus, cfg.JWTSecret, validator)

manager.RestoreActiveGames(ctx)

wsRegistry := ws.NewRegistry()
wsCtx, cancelWSCtx := context.WithCancel(context.Background()) // ADR-018
router := api.NewRouter(manager, userStore, wsRegistry, cfg.JWTSecret, wsCtx)
```

**Graceful shutdown — 5 explicit steps, each bounded by a 15s timeout:**
```go
httpServer.Shutdown(shutdownCtx)      // 1. stop accepting new HTTP/WS
cancelWSCtx()                          // 2. cancel ADR-018 context
wsRegistry.CloseAll()                  // 3. force-close + drain WS connections
manager.PersistActiveClockState(shutdownCtx) // 4. defense-in-depth clock flush
pool.Close()                           // 5. close DB pool
```

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1 | 2025-01-XX | Project scoped, tech stack decided, all documentation created |
| 2 | 2025-01-XX | Documentation corrections only |
| 3 | 2025-01-XX | Step 1 scaffold complete |
| 4 | 2025-01-XX | Steps 2–4 complete: migrations, store layer, auth layer |
| 5 | 2025-01-XX | Step 5 complete: chess layer, ADR-013 |
| 6 | 2026-06-27 | ws port, Steps 6–7 complete |
| 7 | 2026-06-28 | Step 8 complete: MoveProcessor |
| 8 | 2026-06-30 | Step 9–10 complete: Clock, Manager. UUID v4 → v7 switch |
| 9 | 2026-07-01 | Pre-Step-11 hardening: ADR-014, ADR-015, ADR-016 found/fixed/tested |
| 10 | 2026-07-02 to 2026-07-06 | ADR-017, ADR-018. Handler relocated to `internal/api`. Steps 11–12 complete |
| 11 | 2026-07-08 to 2026-07-09 | **Step 12 test gap closed** (`game_handler_test.go`, 12 tests). **`/health` flat response explicitly confirmed.** **Step 13 complete**: `cmd/server/main.go` full wiring, `main_test.go`. **`HandleDisconnect` clock-persist fix** (found via code review before Step 13 was written) — required ctx-taking signature change; new `PersistActiveClockState`; regression tests added. **Manual E2E testing (PHASE_1.md Step 14) began** and found two real bugs missed by the full automated test suite: (1) graceful shutdown's clock-persist write failing on every run with `"context canceled"`, root-caused to ADR-018's cancel-before-CloseAll ordering poisoning the new I/O — fixed via a detached `context.WithoutCancel` + timeout; (2) WebSocket connections never closing after a game ends (observed via resignation) — root-caused to `finalizeGame` never touching connections at all, fixed via new `GameSession.CloseConnections`, deliberately placed in the same goroutine as the `GAME_OVER` send (not `finalizeGame`) after tracing `eventbus.go`'s buffered-channel semantics to find a real ordering race in the naive fix. New regression test `TestWSHandler_GameOver_ClosesConnectionsAfterDelivery` using real dialed WebSocket connections. `Makefile`'s `test-integration` target fixed to run with `-p 1` after user diagnosed a cross-package DB collision from Go's default parallel package test execution. **TD-008 logged** (automatic migrations on startup, accepted for Phase 1, must revisit before Phase 2). **Two ADR candidates flagged, not yet formally logged**: detached-context pattern for cleanup writes; same-goroutine ordering requirement for connection-close-after-terminal-broadcast. |
```