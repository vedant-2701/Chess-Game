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

**Phase 1 — MVP: ✅ COMPLETE**

All 10 PHASE_1.md acceptance criteria verified MET as of this session's formal completion review. See "Phase 1 Completion Record" below for the criterion-by-criterion account.

**Phase 2 (Horizontal Scaling) is the next phase — design is now complete, implementation has not started.** The originally-planned approach (`RedisEventBus` behind the `EventBus` interface) was audited before implementation and rejected — see `DECISIONS_LOG_PHASE_2.md` ADR-021 for why. The accepted design is co-located sessions routed via a Redis-backed ownership/liveness directory, with a resolve-then-connect connection flow — full spec in `phases/current/PHASE_2.md`, full reasoning in `DECISIONS_LOG_PHASE_2.md` ADR-021 through ADR-025. **`RedisEventBus` is not built, in this phase or any other.** `LocalEventBus` remains the permanent implementation. First implementation task per `PHASE_2.md`'s checklist: Step 1, Redis infrastructure (routing directory, not an event bus).

---

## Phase 1 Completion Record

Formal completion review conducted this session, verified by direct source reading (not assumed from prior session notes) via filesystem access across all relevant packages.

| # | Criterion | Status |
|---|-----------|--------|
| 1 | Two players on different machines complete a full game start to checkmate | ✅ MET |
| 2 | Closing/reopening browser mid-game resumes exact state | ✅ MET |
| 3 | Killing the server process and restarting resumes correctly | ✅ MET |
| 4 | Illegal move via WebSocket rejected, board unchanged | ✅ MET |
| 5 | Timeout detected server-side without client involvement | ✅ MET |
| 6 | All tests pass with `go test -race ./...` | ✅ MET (user-confirmed) |
| 7 | No goroutine leaks after a completed game (goleak/pprof) | ✅ MET — see below |
| 8 | `make migrate-down && make migrate-up` succeeds cleanly | ✅ MET |
| 9 | Server logs all errors with gameID and context; no bare `err.Error()` | ✅ MET |
| 10 | Player cannot move out of turn regardless of client input | ✅ MET |

**Criterion #7 required new work this session** — it was NOT met at session start. The only existing `goleak` coverage was scoped to isolated `Clock` unit tests (`internal/game/clock_test.go`); nothing verified the composite teardown of a real completed game (EventBus subscriber goroutine + all three per-connection goroutines for both players + Clock). Closed via `TestWSHandler_GameOver_NoGoroutineLeaks` — see Testing section and Known Sharp Edges for the `goleak.IgnoreCurrent()` pattern this required.

**ADRs formally logged this session**, closing the two items previously flagged in CLAUDE.md's "Pending ADRs" section:
- **ADR-019** — Detached-context pattern for `HandleDisconnect`'s clock-persist write
- **ADR-020** — `GameSession.CloseConnections` same-goroutine ordering requirement

Both are now in `DECISIONS_LOG_PHASE_1.md` in full ADR form. The "Pending ADRs" section that previously lived in this file is retired — both items are resolved.

---

## Completed Work

### Documentation
- [x] Project purpose and scope defined
- [x] Full tech stack decided and rationale documented
- [x] All documentation files created
- [x] Phase 1 spec written (PHASE_1.md)
- [x] Architecture documented (ARCHITECTURE.md)
- [x] All ADRs logged (DECISIONS_LOG_PHASE_1.md) — **through ADR-020, complete**
- [x] Handler package relocation corrected (`internal/ws/handler.go` → `internal/api/ws_handler.go`) across all three docs
- [x] `GET /health`'s flat response shape — confirmed prior session

### Implementation
- [x] WebSocket infrastructure (`internal/ws`): connection lifecycle, read loop, write loop, heartbeats, registry, graceful shutdown
- [x] Steps 1–13: scaffold, migrations, store layer, auth, chess layer, session/registry, EventBus, move pipeline, clock, Manager, WS handler, HTTP handlers, main/wiring
- [x] ADR-014 through ADR-020 — all implemented, tested, and now fully logged
- [x] `HandleDisconnect` clock-persist fix, `PersistActiveClockState`, `GameSession.CloseConnections` — all from prior session, now backed by formal ADRs
- [x] **`TestWSHandler_GameOver_NoGoroutineLeaks` (this session)** — closes acceptance criterion #7

---

## Phase 1 Checklist

### Foundation
- [x] go.mod initialized with all dependencies
- [x] .env.example created
- [x] docker-compose.yml created (PostgreSQL + Redis placeholder)
- [x] Makefile created with standard targets — `test-integration` target runs with `-p 1`
- [x] Directory structure scaffolded

### Database
- [x] Migrations 001–003 (users, games, moves)
- [x] pgxpool connection setup
- [x] Store layer implemented

### Auth Layer
- [x] JWT sign/verify, anonymous userID generation

### Chess Layer
- [x] notnil/chess integration, ValidateMove/ApplyMove split, outcome detection, FEN/SAN handling, state reconstruction helpers

### Game Layer
- [x] GameSession, state machine (ADR-015 corrected), GameRegistry, EventBus/LocalEventBus, MoveProcessor (ADR-013, ADR-014), Manager, RegisterConnection atomicity (ADR-017), HandleDisconnect clock persistence + detached-context fix (ADR-019), CloseConnections + ordering requirement (ADR-020)

### API Layer (HTTP)
- [x] chi router, POST /games, POST /games/:id/join, GET /games/:id, GET /health (flat, confirmed)

### WebSocket Layer
- [x] ws infrastructure, upgrade handler, token validation, connection registration, message routing

### Move Pipeline
- [x] Full 8-step pipeline, TOCTOU re-check (ADR-014), typed rejection errors

### Time Controls
- [x] Server-side clock, start/switch on connect/move, timeout detection, persisted on disconnect (detached-context, ADR-019) and on shutdown (`PersistActiveClockState`)

### Reconnection / Abandonment / Persistence Recovery
- [x] All implemented and unchanged this session — reconnection via token, abandonment semantics per ADR-015, `RestoreActiveGames` via move-replay (never `GameFromFEN`), zombie-ACTIVE correction

### Server Wiring (Step 13)
- [x] Config, pgxpool, automatic migrations (TD-008), full dependency graph, `RestoreActiveGames`, routes, 5-step graceful shutdown

### Testing
- [x] Store, auth, chess, session/registry, EventBus, move pipeline, clock (with goleak), Manager (integration + concurrency), WebSocket handler (httptest-based) — all present from prior sessions
- [x] `game_handler_test.go` — full HTTP-layer coverage (prior session)
- [x] `cmd/server/main_test.go` — config/log-level coverage (prior session)
- [x] **`TestWSHandler_GameOver_NoGoroutineLeaks` (this session)** — end-to-end goroutine-leak verification for a completed game, using `goleak.IgnoreCurrent()` to correctly scope the check within a shared test binary. See Known Sharp Edges.

### Step 14: End-to-End Verification
- [x] All seven manual E2E checks completed in prior session, two real bugs found and fixed (now ADR-019, ADR-020)

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
| ADR-019 | HandleDisconnect clock-persist context cancellation | Detached context: `context.WithTimeout(context.WithoutCancel(ctx), 5s)` |
| ADR-020 | CloseConnections ordering | Same-goroutine, immediately after the GAME_OVER send; never from `finalizeGame` |

**Phase 2 ADRs (ADR-021 through ADR-025) live in `DECISIONS_LOG_PHASE_2.md`, not this file's log (`DECISIONS_LOG_PHASE_1.md`) — numbering is global and continuous across both files, split by phase for readability only.** Summary: ADR-021 co-located sessions over cross-instance state sync (supersedes this table's ADR-010 entry's "RedisEventBus in Phase 2" half); ADR-022 resolve-then-connect over connect-then-relay; ADR-023 two-key Redis ownership/liveness split over one combined key or active HTTP probing; ADR-024 drop eager `RestoreActiveGames`-at-startup for Phase 2; ADR-025 TD-008 resolved via one-shot pre-deploy migration service.

### Implementation Decisions (No ADR Required)

(Unchanged from prior session — see full table in project history. No new implementation-decision-level entries this session; both new decisions were architecturally significant enough to warrant full ADRs, not table entries.)

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
TD-008: Migrations run automatically on server startup | Phase 1 (Step 13) | Fix by: Phase 2
        Must be revisited before Phase 2 ships multiple concurrent instances — advisory-lock
        contention during startup, DDL-privilege blast radius. Resolution mechanism now DECIDED
        (not just "likely"), per DECISIONS_LOG_PHASE_2.md ADR-025: one-shot pre-deploy `migrate`
        service in docker-compose.yml, server replicas gated on
        `depends_on: condition: service_completed_successfully`. STILL OPEN — decided, not yet
        implemented. Closes when PHASE_2.md's Step 9/Step 10 checklist items are done.
```

---

## Non-Negotiable Constraints

These decisions are locked. Do not revisit without a new ADR.

1. **Server is authoritative for all game state.** Client validation is for UX only.
2. **No client timers for time controls.** Server-side clock only.
3. **Every move is persisted before being broadcast.** Persistence is on the critical path.
4. **No Redis in Phase 1.** EventBus interface exists as a seam regardless of what, if anything, sits behind it later — good architecture on its own merits, not contingent on any specific future swap. (Note: Phase 2 does introduce Redis, but as a routing directory, not as `EventBus`'s implementation — `LocalEventBus` remains permanent. See `DECISIONS_LOG_PHASE_2.md` ADR-021.)
5. **No ORM.** Raw SQL via pgx/v5 only.
6. **No global state.** All state passed via dependency injection.
7. **Every I/O function takes context.Context as its first argument.**
8. **ValidateMove before DB write; ApplyMove after DB write succeeds.** (ADR-013)
9. **Check for an existing, well-tested library before writing custom logic for solved problems.**
10. **Any read-then-write sequence spanning two or more statements must be checked for concurrent-caller correctness, not just sequential correctness, before being considered complete.** (ADR-014, ADR-016, ADR-017) Same discipline extended to send-then-close sequences across an async channel (ADR-020).
11. **`internal/api` is the only package permitted to import both `internal/ws` and `internal/game`.**
12. **A context's cancellation timing chosen for one operation must be re-verified for every other operation later wired through the same context — never assumed safe by inheritance.** (ADR-019)
13. **A message-delivery-then-connection-close sequence must happen in the same goroutine that performed the delivery, in program order — never split across goroutines relying on a queue/channel's "accepted" signal as a proxy for "processed."** (ADR-020)

---

## Known Sharp Edges

- **Migrate CLI URL scheme vs. Go startup URL scheme:** `.env.example` uses `postgres://`. `runMigrations` converts to `pgx5://` before calling `migrate.New`.

- **`notnil/chess` `game.MoveHistory()` panics on nil comments slice.** Always use the `chess.MoveHistory(g)` wrapper.

- **`MoveHistory()` returns annotated SAN.** `+` for check, `#` for checkmate.

- **`DetectOutcome` must be called after `ApplyMove`.**

- **`ReadLoop`/`HandleMessage` context lifetime (ADR-018).** Server-lifetime context, cancelled on SIGTERM before `ws.Registry.CloseAll()`.

- **`Manager.HandleDisconnect`'s clock-persist write does NOT use the passed-in `ctx` directly (ADR-019).** Wraps it in `context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`. If you add more I/O to `HandleDisconnect`, check whether it needs the same treatment before assuming `ctx` is safe to use as-is.

- **`GameSession.CloseConnections` must only be called from the same goroutine as, and immediately after, whichever call sent the terminal `GAME_OVER` message (ADR-020).** The two correct call sites are `Manager.startEventSubscriber`'s `MsgTypeGameOver` branch and `Manager.publishGameOver`'s `EventBus`-failure fallback branch. **Never call it from `Manager.finalizeGame`.**

- **`go test ./...` and `go test -tags integration ./...` must be run with `-p 1`.** All integration tests share one real database with no cross-package isolation.

- **`LocalEventBus.Publish`'s buffered channel send does not mean the subscriber has processed the event yet** — only that it was enqueued (or dropped if the buffer, size 8, was full).

- **`LocalEventBus.Publish` holds `mu.RLock()` during the send loop.** Do not "optimise" to snapshot-then-release.

- **`ComputeFENAfterMove` must only be called after `ValidateMove` returned nil for the same `(g, san)` pair.**

- **`ProcessMove` re-checks `session.status == ACTIVE` under `session.mu.Lock()` immediately before `ApplyMove` (ADR-014).**

- **`onAbandonTimeout` branches on `session.IsPlayerConnected(opponentOf(color))` (ADR-015).**

- **`finalizeGame(gameID)` must be called exactly once per game-ending event.** Deliberately does NOT close WebSocket connections (ADR-020).

- **`JoinGame`'s actual correctness guarantee lives in `GameStore.UpdatePlayerBlack`'s SQL, not the Go-level pre-flight check (ADR-016).**

- **`store.ErrGameNotJoinable` is distinct from `store.ErrGameNotFound`.**

- **`goleak.IgnoreTopFunction` in `clock_test.go` exempts exactly one symbol** — re-verify if the pgx dependency version changes.

- **`goleak.VerifyNone` inspects the WHOLE PROCESS's live goroutines, not "goroutines created since this test began" (new this session).** `go test` runs every test in a package sequentially inside one process. A scoped, per-test `goleak.VerifyNone` call — even correctly placed inside only one test function — will still flag every earlier test's legitimately-still-running goroutines (e.g. `TestWSHandler_ValidToken_ReceivesGameState`'s never-finished game) unless `goleak.IgnoreCurrent()` is included as an option. `IgnoreCurrent()` snapshots live goroutines at the moment it's called — and because Go evaluates a deferred call's arguments immediately (only the call is deferred), placing it inside `defer goleak.VerifyNone(t, goleak.IgnoreCurrent(), ...)` at the top of a test function captures the correct "before this test's own work" baseline. **Any future scoped goroutine-leak test in this codebase must include `goleak.IgnoreCurrent()`, or it will false-positive on unrelated tests' intentionally-long-lived goroutines.** See `TestWSHandler_GameOver_NoGoroutineLeaks` for the reference implementation.

- **Package-wide (`TestMain`-level) `goleak` checking was tried for `internal/api` and reverted.** Most tests in this package correctly leave a game non-terminal (they're testing something unrelated to game completion), so a binary-wide check at `TestMain` conflates real leaks with expected, tested-elsewhere-in-the-suite live state. Do not reintroduce this — use a scoped, per-test check with `goleak.IgnoreCurrent()` instead. Also note: `goleak.VerifyTestMain` calls `os.Exit` internally, which would make any code after it (e.g. `pool.Close()`) dead — a second reason not to use it here.

- **`Clock` struct requires `startedAt time.Time`.**

- **`Resume(color Color)` restarts the timer for `c.active`, not `color`.**

- **`Clock.run()` never acquires `c.mu`.**

- **`RestoreActiveGames` must reconstruct boards via `chess.GameFromMoves`, never `chess.GameFromFEN(game.CurrentFEN)`.**

- **`RestoreActiveGames` must detect and correct zombie ACTIVE games.**

- **`gopls` does not index files under non-default build tags (e.g. `//go:build integration`).**

- **`handleTimeout` must never call any `Clock` method** — it's invoked from within the Clock's own background goroutine.

- **`GameSession.RegisterConnection` is the sole place the WAITING→ACTIVE transition can originate from a connection event (ADR-017).**

- **`ws.Connection.Send`/`SendCloseFrame`/`enqueuePing` each use two sequential `select` statements, not one combined three-case `select`.**

- **`ws.Connection.Start(onMessage, onClose)` is the only supported way to launch a connection's goroutines from outside package `ws`.**

- **`internal/api` is the only package that may import both `internal/ws` and `internal/game`.**

---

## WebSocket Message Protocol (Phase 1)

Unchanged this session. See prior content — all constants in `internal/game/messages.go`, full client→server and server→client message catalog, `ABANDONED` outcome pairing rule (ADR-015), connection-closure-after-`GAME_OVER` behavior (ADR-020).

---

## Key Files and Their Responsibilities

Unchanged this session except for the addition below.

```
internal/api/ws_handler_test.go — Invalid-token/valid-token/reconnect tests,
                                  TestWSHandler_GameOver_ClosesConnectionsAfterDelivery,
                                  TestWSHandler_GameOver_NoGoroutineLeaks (this session —
                                  closes acceptance criterion #7; uses goleak.IgnoreCurrent(),
                                  see Known Sharp Edges)
```

All other file responsibilities unchanged from prior session — see full listing in project history.

---

## Chess Layer / GameSession / Clock / Manager / EventBus / WebSocket / API Key Patterns

Unchanged this session — see prior content, all still accurate. No production code was modified this session; only tests and documentation.

---

## Testing Key Patterns (new subsection this session)

**Scoped goroutine-leak test, correctly isolated within a shared test binary:**
```go
func TestX_NoGoroutineLeaks(t *testing.T) {
    // IgnoreCurrent()'s snapshot happens NOW (arguments to a deferred call
    // are evaluated immediately), before any of this test's own setup —
    // capturing every other test's leftover goroutines as baseline.
    defer goleak.VerifyNone(t,
        goleak.IgnoreCurrent(),
        goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
    )
    // ... drive a game to actual completion ...
    // ... explicitly close client connections and any httptest.Server
    //     BEFORE returning — t.Cleanup runs after deferred calls, too late ...
}
```

**Do NOT** put `goleak.VerifyNone`/`goleak.VerifyTestMain` at package `TestMain` level in `internal/api` — reverted this session, see Known Sharp Edges.

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
| 11 | 2026-07-08 to 2026-07-09 | Step 12 test gap closed, `/health` confirmed, Step 13 complete, `HandleDisconnect` clock-persist fix, manual E2E testing found and fixed two real bugs (later formalized as ADR-019, ADR-020) |
| 12 | 2026-07-09 to 2026-07-10 | **Phase 1 completion review conducted** — 9/10 criteria initially MET, criterion #7 (goroutine leaks) found NOT MET and required new work. **ADR-019 and ADR-020 formally logged**, closing prior session's Pending ADRs. **`TestWSHandler_GameOver_NoGoroutineLeaks` written** — required three iterations to get right: (1) scoped-but-no-`IgnoreCurrent()` failed; (2) a `TestMain`-level attempt was tried by the user and correctly reverted after diagnosing it would conflate real leaks with other tests' intentionally-unfinished games; (3) `goleak.IgnoreCurrent()` added to the scoped per-test check, confirmed passing. Neither failure was a production bug — both were `goleak`-in-shared-test-binary scoping issues, now documented as a standing pattern in Known Sharp Edges. **Phase 1 formally declared COMPLETE — all 10/10 acceptance criteria MET.** No production code changed this session; work was entirely test infrastructure and documentation (ADR logging). |