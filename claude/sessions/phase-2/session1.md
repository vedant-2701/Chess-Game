## Session Summary — Implemented and E2E-tested Phase 2 core

### What Was Built

- **`UpdateGameStatus` predicate fix** (`internal/store/game_store.go`): converted to a true compare-and-swap — `WHERE id=$4 AND status=$5`, new `fromStatus` parameter, new `store.ErrGameStatusConflict` sentinel. Updated all 7 production call sites (`manager.go` ×6, `move.go` ×1) plus 3 test-helper call sites across two files (two of which were missed by me and caught/fixed by the user).
- **Step 1 — Redis Infrastructure**: `docker-compose.yml` Redis service, `internal/game/directory.go`'s `NewRedisClient` (ping-verified, fatal on failure), `REDIS_ADDR`/`INSTANCE_ID` required config in `main.go`, `.env.example` updated, `Makefile`'s `docker-up` fixed to include Redis (was silently broken since this step).
- **Step 2 — Routing Directory**: `RoutingDirectory` interface + `RedisDirectory` implementation. `ClaimOwnership` is a single Lua-script atomic compare-and-swap unifying "fresh claim" (`expectedPriorOwner=""`) and "takeover from confirmed-dead owner" into one primitive. `RenewOwnership`/`ReleaseOwnership` are also CAS-gated. `GetOwner`/`SetAlive`/`IsAlive`/`RenewAlive` complete the interface. Full concurrent-claim race test suite (fresh-claim and takeover variants), all passing.
- **Step 3 — GetOrHydrate**: `GameRegistry.GetOrHydrate` via `golang.org/x/sync/singleflight`, coalescing concurrent hydrate-on-miss calls for the same gameID to exactly one execution. **ADR-027**: `hydrateFn` runs under a context detached from whichever caller triggered it (`context.WithTimeout(context.WithoutCancel(ctx), 10s)`), not that caller's own request-scoped context — otherwise one caller's dropped connection would abort hydration for every other caller piggybacking on the same in-flight call.
- **Step 4 — ConnectClaims**: `internal/auth`'s `ConnectClaims`/`SignConnectToken`/`VerifyConnectToken`, `ConnectClaimsTTL = 10s`. Symmetric to `PlayerClaims` (no internal gameID/color enforcement — left to the caller, matching the existing `PlayerClaims` convention I verified first).
- **Step 5 — Resolve Endpoint**: `Manager.ResolveGame` (full algorithm: owner-alive fast path, fresh-claim, dead-owner-takeover, lost-race fallback), new `hydrateGameSession` (unlike startup-only `restoreGame`, must succeed for **any** game status including terminal — required for Step 5/11's reconnect-to-completed-game requirement). Caught and fixed a real goroutine leak risk during design: terminal games must not subscribe to the EventBus (`startEventSubscriber`'s goroutine only exits on a future `GAME_OVER`, which will never come for a finished game). `GET /games/:id/resolve` via `Authorization: Bearer` (deliberately not `?token=`, unlike WSHandler — no browser-WS header constraint applies to a REST call, so no reason to compound TD-001).
- **Step 6 — Heartbeat Ticker**: `Manager.StartHeartbeat`/`stop()`, single 3s ticker (not two independently-scheduled ones — resolved an apparent tension in ADR-023's own text). New `RoutingDirectory.RenewOwnershipBatch` (one Lua-script round-trip across N games, not N round-trips) and `ReleaseAlive`. Graceful-shutdown release wired into `main.go`'s `shutdown()` as new step 5, detached-context pattern again.
- **Step 7 — Edge Proxy**: first `Dockerfile` this project has ever had (Phase 1 never containerized). `nginx.conf`: round-robin REST upstream, static `map`-based `/connect/{instanceLabel}` dereference, WS upgrade headers, long timeouts. `docker-compose.yml` gained `server1`/`server2`/`nginx`, profile-gated (`cluster`) so plain `docker-up` stays Phase-1-compatible.
- **Step 8 — WSHandler Changes**: full rewrite. Route changed from `/ws/game/{id}` to `/connect/{instanceLabel}` (gameID moved entirely into the signed token, no path segment). New instanceLabel-vs-token integrity check. `HandleConnect` gained the `GetOrHydrate` fallback on registry miss. `RestoreActiveGames` finally removed from `main.go`'s startup per ADR-024, now that the fallback genuinely covers it. Deferred Step 5 test (stale `ConnectClaims`) finally implementable and written.
- **Step 9 — TD-008 Resolution**: one-shot `migrate` service (pinned `v4.18.1`, matching `go.mod`), `server1`/`server2` gated on `service_completed_successfully`. New `SKIP_MIGRATIONS` config flag (defaults `false`; only set `true` in cluster profile) since `main.go` is one binary shared by every deployment mode and ADR-025 requires `runMigrations` not merely be harmless but **not invoked** under the cluster path.
- **Step 11 (in progress) — E2E walkthrough**: wrote `phase2_step11_e2e_walkthrough.md` (mirrors `phase1_step14_e2e_walkthrough.md`'s convention) covering all 6 required scenarios with exact commands and "watch for" correctness criteria. User ran the cluster and found real bugs (see below) — this is exactly what Step 11 exists to catch.

### Decisions Made

- **ADR-026** (logged): `github.com/redis/go-redis/v9` as the Redis client — library selection, Phase-1-ADR-002-through-006 convention, not a full design debate.
- **ADR-027** (logged): `GetOrHydrate`'s shared hydration runs under a context detached from the triggering caller, independently bounded — generalizes ADR-019's pattern to a second call site.
- **New ADR needed, not yet written** — see Problems Encountered below: a correction to how `CreateGame`/`JoinGame` handle cross-instance consistency, discovered via Step 11 E2E testing. Should be logged as **ADR-028** once the fix lands.

### Tradeoffs Considered

- `ClaimOwnership`'s single CAS primitive (unifying fresh-claim and takeover via `expectedPriorOwner`) over a separate "force claim" method — closes the split-brain risk at the primitive level, not just at orchestration.
- `RestoreActiveGames` removal deliberately sequenced to Step 8, not done earlier when first noticed stale in Step 6 — removing it before `HandleConnect`'s fallback existed would have been a real regression.
- Terminal-game session eviction (TD-P2-004) explicitly **not** built — no demonstrated need yet, consistent with this project's established discipline (ADR-014, ADR-016, TD-P2-001).
- Single heartbeat ticker at the 3s liveness interval, doing both renewals every tick, over two independently-scheduled tickers — matches ADR-023's literal "two Redis writes per tick" wording.

### Lessons Learned

- **Integration tests within a single-Manager test harness cannot catch cross-instance bugs.** Every `CreateGame`-then-`JoinGame` test this session used the *same* `Manager`/registry instance for both calls, so `JoinGame`'s dependency on the local in-memory registry never failed in any test — it only failed once real nginx round-robin put the two calls on genuinely different processes. This is a real blind spot in how I verified Steps 5–9; only actual multi-instance E2E testing (Step 11) surfaced it.
- **`gopls` was unreliable throughout this session** — stale on filesystem-MCP writes repeatedly, and doesn't index `//go:build integration` files at all. I leaned on it for "final sweep" confidence multiple times when the user's own `go test -tags integration` run was the actual ground truth. Two real bugs (stale `UpdateGameStatus` call sites in `manager_test.go`/`manager_race_test.go`) were caught by the user, not by me, because I trusted a `gopls` reference search that silently excluded integration-tagged files.
- **A design decision that looks sufficient in isolation (`HandleConnect`'s `GetOrHydrate` fallback, Step 8) can leave a sibling code path (`JoinGame`) with the identical unaddressed problem.** I explicitly reasoned about `JoinGame`'s registry-miss gap back in Step 8 and concluded it was "pre-existing, narrow, restart-only" — that reasoning was wrong about *frequency*: under real round-robin, `CreateGame`→`JoinGame` landing on different instances is close to a coin flip, not a rare restart edge case.
- `ConnectClaims`' deliberately short 10s TTL (ADR-022) is a genuine ergonomics problem for *manual* human-paced testing (copy-paste between terminal steps), even though it's working exactly as designed. Worth distinguishing "system bug" from "testing workflow friction" — this session's 401 was the latter, confirmed by the timestamps.

### Problems Encountered

**Two real, unresolved bugs found via Step 11 E2E testing — session ended before either was fixed:**

1. **CRITICAL — `Manager.JoinGame` fails whenever it lands on a different instance than the one that ran `CreateGame`.** Confirmed via live logs: `"Manager.JoinGame: game in DB but not in registry — server may have restarted"`, HTTP 500, `INTERNAL_ERROR`. Root cause: `JoinGame` (`internal/game/manager.go`) does a plain `m.registry.Get(gameID)` and fails outright on a miss — it never received the `GetOrHydrate` fallback `HandleConnect` got in Step 8. Under nginx round-robin across two instances, this is close to a 50% failure rate on ordinary game joins, not a rare restart edge case. **Fix (diagnosed, not yet applied)**: give `JoinGame` the same `GetOrHydrate` fallback pattern as `HandleConnect`, replacing the direct `registry.Get` failure path.

2. **`CreateGame` never claims Redis ownership for the game it just created**, only registers it in the local in-memory registry. Result: the heartbeat ticker (`registry.AllActive()`-driven) tries to renew ownership every 3s for a game that has no ownership record in Redis at all, producing `"heartbeat: lost ownership renewal for game — no longer the recorded owner"` **forever**, for every game that's created but never resolved. Confirmed in the user's logs, indefinitely repeating. **Fix (diagnosed, not yet applied)**: `CreateGame` should call `ClaimOwnership(ctx, gameID, m.instanceID, "")` immediately after creating the session — best-effort/non-fatal (a Redis hiccup at creation time shouldn't fail game creation; a later resolve call would just claim it fresh).

**Not a bug, but flagged**: the 401 the user saw on a WS dial was `CONNECT_TOKEN_EXPIRED` firing correctly — the resolve→dial gap was actually ~11s (confirmed via log timestamps: resolve at `:53`, connect at `:04` the next minute), just over the 10s TTL. `get-id-and-token.sh` does two sequential resolve calls before printing anything, which eats into a human's available window. This is testing-workflow friction, not a system defect — but worth improving the script (or writing a resolve+dial-in-one-shot variant) before the next E2E pass, since it'll keep costing testing time otherwise.

I was mid-way through reading `manager.go`'s `CreateGame`/`JoinGame`/`HandleConnect` in full to design precise fixes for both bugs (already fetched the source, already have the exact fix approach in mind for both) when the session ended. **Neither fix has been applied to any file yet.**

### Checklist Progress

| Item | Status |
|---|---|
| `UpdateGameStatus` predicate fix (Step 10) | ✅ Complete |
| Step 1: Redis Infrastructure | ✅ Complete |
| Step 2: Routing Directory | ✅ Complete |
| Step 3: GetOrHydrate | ✅ Complete |
| Step 4: Auth — ConnectClaims | ✅ Complete |
| Step 5: Resolve Endpoint | 🔄 In Progress — code complete, unit/integration tests pass, but **CreateGame's missing ownership claim (bug #2 above) is functionally part of this step's correctness** |
| Step 6: Heartbeat Ticker | ✅ Complete (code-correct; surfaces bug #2's symptom but isn't itself the bug) |
| Step 7: Edge Proxy / nginx | ✅ Complete, cluster boots and `/health` verified |
| Step 8: WSHandler Changes | 🔄 In Progress — `HandleConnect`'s own fallback is correct and tested, but **the identical gap was missed in `JoinGame` (bug #1 above), which is the more consequential half of "cross-instance consistency" this step was supposed to establish** |
| Step 9: TD-008 Resolution | ✅ Complete |
| Step 10: Bug Fix Carried In From Audit | ✅ Complete (done first, before Step 1) |
| Step 11: Multi-Instance Testing | 🔄 In Progress — Scenario 0 (co-location) blocked by bug #1; found 2 real bugs, 0 of 6 scenarios cleanly passed yet |
| Step 12: Documentation | ⬜ Not started (this document is part of it) |

### Technical Debt Introduced

- **CRITICAL-1 (not TD, active bug)**: `Manager.JoinGame` has no `GetOrHydrate` fallback — fails on any cross-instance registry miss. | Introduced: Phase 1 (latent), exposed: Phase 2 Step 11 | Must fix: immediately, next session, before Step 11 can proceed at all.
- **CRITICAL-2 (not TD, active bug)**: `Manager.CreateGame` never claims Redis ownership at creation time, causing indefinite heartbeat warning-log spam for any created-but-never-resolved game. | Introduced: Phase 2 Step 5 | Must fix: immediately, next session.
- TD-P2-001 (existing, ADR-023): no fencing token, narrow TTL-bounded false-positive liveness window. | Phase 2 | Fix by: Phase 8.
- TD-P2-002 (existing): static Edge Proxy config, requires manual reload to add an instance. | Phase 2 | Fix by: Phase 8.
- TD-P2-003 (existing): no active liveness probing, purely TTL-based. | Phase 2 | Revisit only if TD-P2-001 shown to matter.
- **TD-P2-004** (new, this session, flagged in `resolve.go`'s doc comment): a session hydrated via resolve for an already-terminal game stays registered indefinitely — nothing currently evicts it. | Phase 2 Step 5 | Not scheduled; revisit if shown to matter.
- **TD-P2-005** (new, should be logged — not yet added to CLAUDE.md's table before this session ended): `onAbandonTimeout`'s both-disconnected branch can never succeed for a `WAITING_FOR_PLAYER` game (no `WAITING→ABANDONED` edge exists) — a creator who disconnects before anyone joins and never returns leaves the game stuck in `WAITING_FOR_PLAYER` forever. Flagged during the `UpdateGameStatus` fix at session start, never fixed. | Phase 1 (pre-existing) | Not scheduled.

### Files Modified

`internal/store/errors.go`, `internal/store/game_store.go`, `internal/store/game_store_test.go`, `internal/game/testmain_test.go`, `internal/game/manager_test.go` (user-fixed), `internal/game/manager_race_test.go` (user-fixed), `internal/game/manager.go`, `internal/game/move.go`, `internal/game/errors.go`, `internal/game/directory.go` (new), `internal/game/directory_test.go` (new), `internal/game/registry.go`, `internal/game/registry_test.go`, `internal/game/resolve.go` (new), `internal/game/resolve_test.go` (new), `internal/game/heartbeat.go` (new), `internal/game/heartbeat_test.go` (new), `internal/game/handleconnect_hydrate_test.go` (new), `internal/game/messages.go`, `internal/auth/token.go`, `internal/auth/token_test.go`, `internal/api/game_handler.go`, `internal/api/game_handler_test.go`, `internal/api/routes.go`, `internal/api/ws_handler.go` (full rewrite), `internal/api/ws_handler_test.go` (full rewrite), `internal/api/resolve_test.go` (new), `internal/api/testmain_test.go`, `cmd/server/main.go`, `cmd/server/main_test.go`, `docker-compose.yml`, `Dockerfile` (new), `nginx.conf` (new), `Makefile`, `.env.example`, `get-id-and-token.sh`, `phase2_step11_e2e_walkthrough.md` (new), `claude/claude_web_project/DECISIONS_LOG_PHASE_2.md` (ADR-026, ADR-027 appended).

### Recommended Next Step

**Fix `Manager.CreateGame` and `Manager.JoinGame` in `internal/game/manager.go`, in that order:**

1. `CreateGame`: after `m.registry.Register(session)`, add a best-effort `if m.directory != nil { if _, err := m.directory.ClaimOwnership(ctx, gameID, m.instanceID, ""); err != nil { slog.Error(...) } }` — non-fatal, matching how Redis-down must never break gameplay-adjacent operations.
2. `JoinGame`: replace the direct `session, err := m.registry.Get(gameID)` failure path with the same `GetOrHydrate` fallback `HandleConnect` already has (Step 8) — reuse `hydrateGameSession` via `m.registry.GetOrHydrate(ctx, gameID, func(hydrateCtx context.Context) (*GameSession, error) { return m.hydrateGameSession(hydrateCtx, gameID) })`.
3. Write regression tests for both (a cross-instance `JoinGame` test using two separate `Manager` instances sharing the same `testPool`/`testRedisClient`, and a `CreateGame`-then-check-`GetOwner` test).
4. Log **ADR-028** documenting this correction — same shape as ADR-024/Step-8's `HandleConnect` fix, just discovered one step later than it should have been.
5. Re-run `phase2_step11_e2e_walkthrough.md` Scenario 0 from the top to confirm co-location actually works before proceeding to Scenarios 1–5.

Estimated: 1–2 hours including tests.

---

```markdown
# CLAUDE.md — Session Context Document

This file is the authoritative context document for AI-assisted development sessions on this project.

**Read this first. Every session. Before writing any code.**

Update this file at the end of every session. Stale context is worse than no context.

---

## Project Identity

**Name:** chess-server
**Module path:** `github.com/vedant-2701/chess`
**Language:** Go 1.25+
**Type:** Learning project — production-grade chess platform, phase-by-phase
**Primary Goal:** Learn system design, distributed systems, real-time backend architecture
**NOT a goal:** Build a Chess.com competitor

---

## Current Phase

**Phase 1 — MVP: ✅ COMPLETE**

**Phase 2 (Horizontal Scaling): 🔄 IN PROGRESS — implementation Steps 1–10 code-complete, Step 11 (Multi-Instance E2E Testing) in progress with two unresolved critical bugs found.**

**Read this before doing anything else this session:** Phase 2's own implementation has a confirmed, reproducible, severe bug in `Manager.JoinGame` and a second, confirmed bug in `Manager.CreateGame`. Both were found via real multi-instance E2E testing (not caught by any unit/integration test, because every existing test uses a single shared `Manager` instance for both `CreateGame` and `JoinGame`, which structurally cannot reproduce a cross-instance registry miss). See "Critical Open Bugs" below — fixing these is the first task of the next session, before any further Step 11 scenarios are attempted.

---

## Critical Open Bugs (fix these first, next session)

### Bug 1 (CRITICAL): `Manager.JoinGame` fails whenever it lands on a different instance than `CreateGame`

**Confirmed via live cluster logs:**
```
level=ERROR msg="Manager.JoinGame: game in DB but not in registry — server may have restarted" gameID=... userID=...
level=ERROR msg="GameHandler.JoinGame: game exists in DB but has no in-memory session" ...
```
HTTP 500, `INTERNAL_ERROR`, `BLACK_TOKEN=null` downstream in test scripts.

**Root cause:** `internal/game/manager.go`'s `JoinGame` does a plain `session, err := m.registry.Get(gameID)` and returns an error outright on a registry miss. Under nginx's round-robin REST routing (`docker-compose.yml`'s `chess_rest` upstream), `POST /games` (CreateGame) and `POST /games/:id/join` (JoinGame) are two independent REST calls that land on `server1`/`server2` essentially at random — close to a 50% chance of mismatch with only two instances. This is **not** a rare restart edge case (which is how I originally, incorrectly, reasoned about this gap back in Step 8 — see Lessons Learned in the last session summary) — it is close to the *common* case.

`Manager.HandleConnect` already received the correct fix for the analogous problem in Step 8 (`registry.GetOrHydrate` fallback, reusing `hydrateGameSession`). `JoinGame` needs the identical treatment; it was missed.

**Fix, diagnosed but not yet applied:**
```go
session, err := m.registry.GetOrHydrate(ctx, gameID, func(hydrateCtx context.Context) (*GameSession, error) {
    return m.hydrateGameSession(hydrateCtx, gameID)
})
if err != nil {
    return "", fmt.Errorf("Manager.JoinGame gameID=%s: %w", gameID, err)
}
```
replacing the current direct `m.registry.Get(gameID)` + error-return block. `hydrateGameSession` (in `internal/game/resolve.go`) already handles any game status correctly, including `WAITING_FOR_PLAYER` (the actual status a not-yet-joined game will have).

### Bug 2: `Manager.CreateGame` never claims Redis ownership at creation time

**Confirmed via live cluster logs, repeating every ~3s indefinitely:**
```
level=WARN msg="heartbeat: lost ownership renewal for game — no longer the recorded owner" gameID=... instanceID=server1
```
for games that were created but never resolved.

**Root cause:** `CreateGame` calls `m.registry.Register(session)` (correct — the game genuinely is live on the creating instance) but never calls `m.directory.ClaimOwnership(...)`. The heartbeat ticker (`heartbeatTick`, `internal/game/heartbeat.go`) renews ownership for every session in `registry.AllActive()` regardless of whether that session ever had a Redis ownership record in the first place. `RenewOwnership`'s CAS fails (correctly) when the key doesn't exist at all, producing the "lost ownership" warning — forever, since nothing ever creates the missing key.

**Fix, diagnosed but not yet applied:** in `CreateGame`, immediately after `m.registry.Register(session)`:
```go
if m.directory != nil {
    if _, err := m.directory.ClaimOwnership(ctx, gameID, m.instanceID, ""); err != nil {
        slog.Error("CreateGame: failed to claim initial ownership", "gameID", gameID, "error", err)
        // non-fatal — a later resolve call will claim it fresh; Redis being
        // briefly down at creation time must not fail game creation itself.
    }
}
```

**Both fixes need regression tests using two separate `Manager` instances sharing the same `testPool`/`testRedisClient`** (mirroring `internal/game/resolve_test.go`'s `newTestManagerWithDirectory` pattern) — a single-`Manager` test cannot reproduce either bug, which is exactly how both went unnoticed until real E2E testing.

**Once fixed:** log **ADR-028** (same shape as the Step 8 `HandleConnect` fix / ADR-024) documenting this correction, then re-run `phase2_step11_e2e_walkthrough.md` Scenario 0 from the top.

---

## Phase 1 Completion Record

(Unchanged from prior sessions — all 10 acceptance criteria MET. See prior CLAUDE.md revisions or `DECISIONS_LOG_PHASE_1.md` for full detail.)

---

## Phase 2 Implementation Status

**Architecture:** Co-located sessions, Redis-backed ownership/liveness routing directory, resolve-then-connect connection flow. Full reasoning: `DECISIONS_LOG_PHASE_2.md` ADR-021 through ADR-027 (ADR-028 pending — see Critical Open Bugs above). Full spec: `phases/current/PHASE_2.md`.

### Step-by-step status

| Step | Description | Status |
|---|---|---|
| — | `UpdateGameStatus` predicate fix (PHASE_2.md Step 10, done first) | ✅ Complete |
| 1 | Redis Infrastructure | ✅ Complete |
| 2 | Routing Directory (`RoutingDirectory`/`RedisDirectory`) | ✅ Complete |
| 3 | `GameRegistry.GetOrHydrate` (singleflight) | ✅ Complete |
| 4 | Auth — `ConnectClaims` | ✅ Complete |
| 5 | Resolve Endpoint (`Manager.ResolveGame`, `GET /games/:id/resolve`) | 🔄 In Progress — Bug 2 above is functionally part of this step |
| 6 | Heartbeat Ticker | ✅ Complete (code-correct; surfaces Bug 2's symptom, isn't the bug itself) |
| 7 | Edge Proxy / nginx | ✅ Complete — cluster boots, `/health` verified working |
| 8 | WSHandler Changes (`ConnectClaims`, `/connect/{instanceLabel}`, `HandleConnect` fallback) | 🔄 In Progress — Bug 1 above is the missed half of this step's actual scope |
| 9 | TD-008 Resolution (one-shot `migrate` service) | ✅ Complete |
| 10 | Bug Fix Carried In From Audit | ✅ Complete |
| 11 | Multi-Instance Testing | 🔄 In Progress — 0 of 6 scenarios cleanly passed; blocked by Bugs 1 and 2 |
| 12 | Documentation | ⬜ Not started |

### What's implemented and working (verified by real `go test -race`/`-tags integration` runs, and — where noted — a live cluster)

- Redis client + `RoutingDirectory`/`RedisDirectory`: `ClaimOwnership` (single Lua-script CAS unifying fresh-claim and dead-owner-takeover), `GetOwner`, `RenewOwnership`, `ReleaseOwnership`, `SetAlive`, `IsAlive`, `RenewAlive`, `RenewOwnershipBatch` (Step 6), `ReleaseAlive` (Step 6). Concurrent-claim race tests, all passing.
- `GameRegistry.GetOrHydrate`: `golang.org/x/sync/singleflight`-based, detached-context hydration (ADR-027).
- `internal/auth`: `ConnectClaims`/`SignConnectToken`/`VerifyConnectToken`, `ConnectClaimsTTL = 10s`.
- `Manager.ResolveGame` + `hydrateGameSession` (handles any game status, including terminal — reconnect-to-completed-game works, verified via `go test` and confirmed manually in this session's cluster testing).
- `Manager.StartHeartbeat`/`stop()`: single 3s ticker, batched ownership renewal + liveness renewal, graceful-shutdown release wired into `main.go`.
- `GET /games/:id/resolve`: `Authorization: Bearer` auth (not `?token=` — deliberate, avoids compounding TD-001 on a new endpoint with no browser-WS header constraint).
- nginx Edge Proxy: round-robin REST, static `map`-based `/connect/{instanceLabel}` dereference. Cluster boots cleanly via `make docker-up-cluster`, `/health` returns 200 through nginx.
- `WSHandler`: fully rewritten for `ConnectClaims` at `/connect/{instanceLabel}` (old `/ws/game/{id}` route removed entirely, not kept in parallel — matches this project's "single correct code path" discipline). `HandleConnect`'s `GetOrHydrate` fallback verified working via manual cluster testing (Scenario 5-equivalent: resolved and connected to a game whose session had been hydrated fresh).
- `SKIP_MIGRATIONS` config flag + one-shot `migrate` service: TD-008 closed.

### What's confirmed broken (see Critical Open Bugs above)

- `JoinGame` fails ~50% of the time under real round-robin (Bug 1).
- `CreateGame`-created-but-never-resolved games produce indefinite heartbeat log spam (Bug 2).

### Not yet exercised (blocked by the above, or simply not reached yet)

- Failover (Scenario 1), split-brain regression (Scenario 2), one-sided-abandonment (Scenario 3), Redis-down-mid-game (Scenario 4) — `phase2_step11_e2e_walkthrough.md` scenarios not yet cleanly run end-to-end due to Bug 1 blocking Scenario 0.

---

## Architectural Decisions (Summary)

Full rationale in `DECISIONS_LOG_PHASE_1.md` (ADR-001–020) and `DECISIONS_LOG_PHASE_2.md` (ADR-021 onward, numbering continuous across both files).

**Phase 1:** ADR-001 through ADR-020 — unchanged, see prior CLAUDE.md revisions.

**Phase 2 (`DECISIONS_LOG_PHASE_2.md`):**

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-021 | Cross-instance architecture | Co-located sessions + Redis ownership/liveness directory, not cross-instance state sync |
| ADR-022 | Connection flow | Resolve-then-connect, two-token split (`PlayerClaims` + `ConnectClaims`), not connect-then-relay |
| ADR-023 | Redis key design | Two separate keys (ownership vs. liveness), not one combined key or active HTTP probing |
| ADR-024 | Startup restore | Drop eager `RestoreActiveGames`; `GetOrHydrate` is now the single mechanism for local registry misses (implemented Step 8) |
| ADR-025 | TD-008 resolution | One-shot pre-deploy `migrate` service, `SKIP_MIGRATIONS` flag (implemented Step 9) |
| ADR-026 | Redis client library | `github.com/redis/go-redis/v9` (Phase-1-convention-style library pick) |
| ADR-027 | `GetOrHydrate` context handling | Detached from triggering caller, independently bounded — generalizes ADR-019 to a second call site |
| **ADR-028** | **Pending — not yet written** | Cross-instance consistency fix for `CreateGame`/`JoinGame`, mirroring the `HandleConnect` fix from Step 8. Write this once Bugs 1 and 2 above are fixed. |

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
TD-008: Migrations run automatically on server startup | CLOSED (Phase 2 Step 9) — one-shot migrate service + SKIP_MIGRATIONS flag. See ADR-025.

TD-P2-001: No fencing token — narrow, TTL-bounded false-positive liveness window can theoretically split a live game | Phase 2 | Fix by: Phase 8 (k8s Lease)
TD-P2-002: Static Edge Proxy config — scaling requires a config edit + reload | Phase 2 | Fix by: Phase 8 (dynamic service discovery)
TD-P2-003: No active liveness probing — detection is purely TTL-based | Phase 2 | Revisit only if TD-P2-001's window is shown to matter
TD-P2-004: A session hydrated via resolve for an already-terminal game stays registered in the local GameRegistry indefinitely — nothing currently evicts it (reopens, narrowly, the unbounded-growth concern finalizeGame was built to prevent) | Phase 2 Step 5 | Not scheduled; revisit if shown to matter
TD-P2-005: onAbandonTimeout's both-disconnected branch can never succeed for a WAITING_FOR_PLAYER game (no WAITING→ABANDONED edge in validTransitions) — a creator who disconnects before anyone joins and never returns leaves the game stuck in WAITING_FOR_PLAYER forever. Pre-existing since Phase 1, flagged not fixed. | Phase 1 (pre-existing) | Not scheduled

CRITICAL-1 (active bug, not accepted debt): Manager.JoinGame has no GetOrHydrate fallback — fails on any cross-instance registry miss, ~50% of joins under real round-robin. | Phase 2 | Fix: immediately, next session
CRITICAL-2 (active bug, not accepted debt): Manager.CreateGame never claims Redis ownership at creation time, causing indefinite heartbeat log spam for created-but-never-resolved games. | Phase 2 | Fix: immediately, next session
```

---

## Non-Negotiable Constraints

Unchanged from prior sessions (1–13), still locked. See prior CLAUDE.md revisions for the full list. Notably still governing this session's work:

12. **A context's cancellation timing chosen for one operation must be re-verified for every other operation later wired through the same context — never assumed safe by inheritance.** (ADR-019) This constraint was invoked a second time this session for `GetOrHydrate`'s hydration context (ADR-027) and for `StartHeartbeat`'s shutdown-release path — both correctly detached, both documented at their call sites.

**New this session, not yet formalized as a numbered constraint but worth treating as one going forward:** *Any fix applied to one code path handling a shared cross-cutting concern (e.g., "recover from a local registry miss") must be checked against every other code path with the same structural dependency, not assumed to be the only place it's needed.* This is exactly the category of bug Bugs 1 and 2 both are — `HandleConnect` got the `GetOrHydrate` fix in Step 8; `JoinGame` had the identical need and was missed. Consider formalizing this as Constraint #14 next session.

---

## Known Sharp Edges

(All prior sharp edges from earlier sessions remain valid — see prior CLAUDE.md revisions for the full list: migrate CLI URL scheme, `notnil/chess` panics, `ReadLoop` context lifetime, `HandleDisconnect`'s detached clock-persist context, `CloseConnections` ordering requirement, goleak `IgnoreCurrent()` pattern, etc.)

**New this session:**

- **`gopls` (via the gopls MCP server) is unreliable for verifying edits made through the Filesystem MCP server.** Repeatedly showed stale content after `Filesystem:edit_file`/`Filesystem:write_file` calls — sometimes for many tool calls in a row. **Never trust a `gopls` diagnostics/reference-search result over directly re-reading the file's actual content when verifying an edit landed correctly.** The user's own `go build`/`go vet`/`go test` output is the only fully authoritative signal.
- **`gopls` does not index files under `//go:build integration`** (already known from Phase 1, reconfirmed repeatedly this session). A `gopls:go_symbol_references` search for `NewManager`/`UpdateGameStatus`/etc. will silently miss every call site in an integration-tagged test file. When checking "did I update every call site of X," grep/read integration-tagged test files **directly** — do not rely on any gopls-based search to be complete.
- **Redis client test isolation via DB 1** (not `FLUSHDB` against whatever a developer has on DB 0) — same principle as `testPool` connecting to `chess_dev` instead of `chess`. Established in `internal/game/testmain_test.go` and mirrored in `internal/api/testmain_test.go`.
- **`Connection.Close()` (internal/ws/connection.go) calls `c.wsConn.Close()` unconditionally** — will nil-pointer-panic if constructed via `ws.NewConnection(id, nil)` for a test. `Send()` is safe with a nil `wsConn` (only enqueues onto an internal channel; never touches `wsConn` unless `WriteLoop` — started only via `conn.Start()` — is actually running). Verified directly against source this session after nearly shipping a test that would have panicked.
- **`ConnectClaimsTTL` (10s) is tight for manual/human-paced testing** (curl + copy-paste between resolve and dial). Not a bug — confirmed via log timestamps that a real 401/`CONNECT_TOKEN_EXPIRED` firing at ~11s elapsed is the system working as designed — but `get-id-and-token.sh` doing two sequential resolve calls before printing anything eats into the available window. Consider a resolve-and-dial-immediately script variant before the next E2E pass.
- **Cross-instance bugs cannot be caught by single-`Manager` integration tests.** Any future test asserting behavior that's supposed to work "across instances" (routing, ownership handoff, registry-miss recovery) must construct **two separate `Manager` instances** sharing the same `testPool`/`testRedisClient` — one `Manager` instance structurally cannot reproduce what nginx's round-robin does in production. This is exactly how Bugs 1 and 2 went unnoticed through all of Steps 5–9's own test suites.

---

## WebSocket Message Protocol (Phase 2 — changed this session)

**Route changed**: `GET /ws/game/{id}?token=<playerToken>` (Phase 1) → `GET /connect/{instanceLabel}?token=<connectToken>` (Phase 2 Step 8). The old route is **removed entirely**, not kept in parallel — per this project's "single correct code path" discipline and ADR-022's explicit "every connect or reconnect uses the identical path, no special-casing."

- `gameID`/`userID`/`color` now come entirely from the verified `ConnectClaims` token, **not** the URL — the masked URL deliberately has no gameID path segment.
- New integrity check: `claims.InstanceLabel` must match the `:instanceLabel` URL parameter.
- New error code `ErrCodeConnectTokenExpired` (`CONNECT_TOKEN_EXPIRED`), distinct from generic `ErrCodeInvalidToken` — tells the client to re-resolve, not retry the same masked URL.
- Client flow is now two-step: `GET /games/:id/resolve` (Authorization: Bearer `<playerToken>`) → `{connectToken, instanceLabel, wsPath}` → dial `wsPath + "?token=" + connectToken` within `ConnectClaimsTTL` (10s).

All other message types/payloads (`GAME_STATE`, `MOVE`, `MOVE_APPLIED`, `MOVE_REJECTED`, `RESIGN`, `GAME_OVER`, `PING`/`PONG`, `OPPONENT_*`) are unchanged from Phase 1.

---

## Key Files and Their Responsibilities (Phase 2 additions this session)

```
internal/game/directory.go       — NewRedisClient, RoutingDirectory interface, RedisDirectory impl
internal/game/directory_test.go  — concurrent-claim race tests, CAS behavior tests
internal/game/resolve.go         — Manager.ResolveGame, hydrateGameSession
internal/game/resolve_test.go    — ResolveGame tests incl. terminal-game hydration + goleak check
internal/game/heartbeat.go       — Manager.StartHeartbeat, heartbeatTick, releaseHeartbeatEntries
internal/game/heartbeat_test.go  — heartbeat lifecycle tests incl. goleak check
internal/game/handleconnect_hydrate_test.go — HandleConnect's GetOrHydrate fallback, direct test
internal/api/resolve_test.go     — HTTP-level resolve endpoint tests
internal/api/ws_handler.go       — FULLY REWRITTEN for ConnectClaims/instanceLabel (Step 8)
internal/api/ws_handler_test.go  — FULLY REWRITTEN to match
Dockerfile                       — NEW, first container image this project has had
nginx.conf                       — NEW, Edge Proxy config
phase2_step11_e2e_walkthrough.md — NEW, manual multi-instance testing script (mirrors phase1_step14_e2e_walkthrough.md)
```

All other Phase 1 file responsibilities unchanged — see prior CLAUDE.md revisions for the full listing.

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1–12 | 2025-01-XX to 2026-07-10 | Phase 1 complete (see prior CLAUDE.md revisions for full detail) |
| 13 | 2026-07-15 | Phase 2 architecture design: audited original Redis pub/sub plan, found critical bugs, rejected ID-prefix and consistent-hash-ring alternatives, arrived at co-located sessions + Redis routing directory. ADR-021 through ADR-025 written. |
| 14 | 2026-07-21 | **Phase 2 implementation, Steps 1–10 + Step 11 (in progress).** `UpdateGameStatus` predicate fix done first (CAS, new `ErrGameStatusConflict` sentinel, all 7 production + 3 test call sites — 2 missed by me, caught by user). Steps 1–9 fully implemented: Redis infra, `RoutingDirectory`/`RedisDirectory` (Lua-script CAS), `GetOrHydrate` (singleflight, ADR-027 detached context), `ConnectClaims` auth, `Manager.ResolveGame`/`hydrateGameSession`, heartbeat ticker (`RenewOwnershipBatch`, `ReleaseAlive`), Dockerfile + nginx Edge Proxy + docker-compose cluster profile, `WSHandler` full rewrite (`/connect/{instanceLabel}`, `RestoreActiveGames` removed per ADR-024), TD-008 closed (`migrate` service + `SKIP_MIGRATIONS`). ADR-026, ADR-027 logged. Wrote `phase2_step11_e2e_walkthrough.md`. **User ran real multi-instance E2E testing and found two critical, unresolved bugs**: `JoinGame` fails ~50% of the time under round-robin (no `GetOrHydrate` fallback, unlike `HandleConnect`), and `CreateGame` never claims Redis ownership at creation time (indefinite heartbeat log spam). Both fixes diagnosed but **not yet applied** — session ended mid-fix. Also flagged, not fixed: pre-existing `onAbandonTimeout` `WAITING→ABANDONED` gap (TD-P2-005). Key lesson: single-`Manager` integration tests structurally cannot catch cross-instance bugs; `gopls` was unreliable throughout (stale on filesystem-MCP writes, blind to integration-tagged files) and should not be trusted over direct file reads or the user's own `go test` output. |
```