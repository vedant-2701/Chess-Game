## PART 1 — SESSION SUMMARY

## Session Summary — Phase 3 Steps 1 Through 3

### What Was Built

**Step 1 — DB layer for matchmaking idempotency:**
- Migration 005: `games.matchmaking_request_id UUID UNIQUE`, nullable
- `GameStore.CreateMatchedGame` (atomic two-player `INSERT ... ON CONFLICT (matchmaking_request_id) DO NOTHING`), `GameStore.GetGameByMatchmakingRequestID`
- Full test coverage in `game_store_test.go` (fresh insert, idempotent retry, both validation-error paths)

**Step 2 — gRPC contract + nginx:**
- `proto/matchmakingv1/matchmaking.proto`: `MatchReportService` (`ReportMatchCreated`, `ReportMatchmakingFailed`), `MatchmakingFailureReason` enum. Codegen run by user (`make install-proto-tools && make proto`) — `.pb.go`/`_grpc.pb.go` now on disk.
- `internal/rpc/interceptor.go`: shared-secret gRPC interceptor (client + server halves), `internal/rpc/interceptor_test.go`
- `nginx.conf`: new `/matchmaking` location block, resolver-based dynamic `proxy_pass` (matchmaking-service has no `docker-compose.yml` entry yet, so a static `upstream{}` block would fail nginx startup)
- `Makefile`: `proto`/`install-proto-tools` targets

**ADR-041/042 — closes TD-P3-004 (first-move grace period + opponent-never-connects timeout):**
- Migration 006: `games.activated_at`; `GameStore.ActivateGame` (atomic `WAITING→ACTIVE` + timestamp)
- `session.go`: new `ACTIVE→ABORTED` edge (retroactive Phase 1/2 behavior change, explicitly confirmed with user before implementing)
- `manager.go`: generalized timer plumbing (`armTimer`/`cancelTimer`) shared by the ordinary abandon timer and two new timer kinds; `onFirstMoveTimeout`, `onOpponentNeverConnectedTimeout`, `onMovePersisted`; `armTimersForGameStatus`/`armTimersForGame` replace `armAbandonTimersForGame` at all three hydration call sites
- `move.go`: `MoveProcessor.onMovePersisted` hook (mirrors the existing `Clock→Manager` callback pattern), wired once in `NewManager`
- `Manager.CreateMatchedGame`: atomic insert, `GameRegistry` registration, `ClaimOwnership`, arms `matchedOpponentConnectTimeout`; idempotent-retry path via `GetGameByMatchmakingRequestID` + `GetOrHydrate`
- Fixed two categories of resulting Phase 1/2 test breakage: `session_test.go` (state-machine edge moved from invalid to valid), `resolve_test.go` (two crash/failover abandon-timer tests needed to play two moves before simulating the crash, since they test a mechanism ADR-041 now gates behind move count ≥ 2)

**Step 3 — `internal/matchmaking` package:**
- `pairing.go`: `PairingLoop` (`ZCARD`/`ZPOPMIN`-driven `tick`, `Start`/`stop` mirroring `StartHeartbeat`), `MatchReporter` interface, `pairAndReport` (bounded retries sharing one `matchmakingRequestID`), `reenqueue`
- `reporter.go`: `grpcMatchReporter`, the concrete `MatchReporter`, context-aware bounded retry with backoff
- `testmain_test.go`, `pairing_test.go`: integration tests including `TestPairingLoop_ConcurrentTicks_NoDoubleMatch` — two real `Manager`s racing `tick()` concurrently against shared Redis+Postgres, this phase's actual learning objective under direct test

**`matchmaking_active_game:{userID}` marker (ADR-037):**
- `directory.go`: `ActiveGameMarker`, `SetActiveGameMarker`/`RenewActiveGameMarkersBatch` on `RoutingDirectory`/`RedisDirectory` + tests
- `heartbeat.go`: `renewActiveGameMarkers`, folded into `heartbeatTick` — re-signs a fresh token per player per tick rather than storing tokens on `GameSession`
- `manager.go`: eager marker writes at `CreateGame`, `JoinGame` (via a `GetOwner` lookup for the correct `instanceLabel`), and both branches of `CreateMatchedGame`

**Wiring:**
- `cmd/server/main.go`: optional `PairingLoop`+`grpcMatchReporter` construction/start/stop, gated on `MATCHMAKING_SERVICE_ADDR`
- `.env.example`: three new optional vars

All confirmed via `go build`, `go vet`, `go test`, `go test -race`, `go test -tags integration -race -p 1`, and `gofmt -l` — all clean, per user-run output.

### Decisions Made

- **Module structure**: `matchmaking-service` builds into its own container but lives in the same `github.com/vedant-2701/chess` Go module (user's explicit call, not formalized as a numbered ADR per user instruction — resolves `internal/auth` importability for `MatchmakingClaims`).
- **`GameStore.ActivateGame`** as a dedicated method rather than an optional field on `UpdateGameStatus` — every other call site of that method transitions to a terminal status and has no meaningful `activated_at`.
- **`MatchReporter` as an interface**, not a concrete gRPC type, in `pairing.go` — decoupled `PairingLoop`'s tick logic from the generated protobuf stubs, which didn't exist on disk at the time. Should be flagged as a durable pattern worth keeping even now that the stubs exist.
- **Heartbeat re-signs tokens fresh each tick** rather than storing them on `GameSession` — a real structural finding, not a style choice: `JoinGame` deliberately never touches a live `GameSession` (may run on a different instance), so a session-held-token design has no way to populate Black's token in that path at all.
- **`JoinGame`'s marker `instanceLabel`** sourced via a `GetOwner` lookup, not just `m.instanceID` — `JoinGame` can land on a different instance than the one hosting the game; using the wrong label would just push a client to `/resolve` (harmless) but a lookup costs little and is more often correct.
- **Phase 3 config is entirely optional** (`MATCHMAKING_SERVICE_ADDR` empty ⇒ pairing loop disabled) — every existing single-instance and Phase 2 deployment is unaffected by default.

### Tradeoffs Considered

- Single-module vs. second `go.mod` for matchmaking-service — single module won; a second module would force `MatchmakingClaims` out of `internal/auth` (Go's `internal/` visibility is enforced at the module boundary), duplicating signing code.
- Re-signing tokens per heartbeat tick vs. storing originals on `GameSession` — re-signing won, for the structural reason above, not performance.
- Plain `SET` vs. compare-and-swap for `ActiveGameMarker` — plain `SET` won; unlike ownership keys, there is no second legitimate writer per `userID` to race against.
- Static nginx `upstream{}` block vs. resolver-based dynamic `proxy_pass` — dynamic won; matchmaking-service has no `docker-compose.yml` entry yet, and a static block resolves hostnames at nginx startup, which would take down `/games` and `/connect` too.

### Lessons Learned

- ADR-041's retroactive Phase 1/2 test breakage was real and exactly the shape predicted when confirmation was requested before implementing — worth treating "this will break existing tests" flags in future ADRs as reliable, not cautious over-statement.
- `JoinGame`'s cross-instance constraint (established in Phase 2) has ripple effects into designs that don't obviously touch `JoinGame` at all — the active-game marker design had to route around it. Worth checking this constraint explicitly whenever a new feature needs anything from a live `GameSession`.
- **A tool call can fail silently in a way that looks like partial success.** One `edit_file` call with two edits failed entirely (atomic, both-or-nothing) when the second edit's text didn't match — but this wasn't caught until a later, unrelated re-read of the file showed the first edit hadn't landed either. Worth re-reading a file after any multi-edit call whose full diff wasn't shown back, not just assuming "no error" means "fully applied" when a tool's atomicity semantics aren't already known.

### Problems Encountered

- Filesystem MCP server had a hard outage mid-session (two consecutive timeouts reading `resolve.go`) — required pausing and asking the user to restart it. Work resumed cleanly afterward with no lost state, since nothing had been written mid-outage.
- Two real implementation bugs caught before shipping, not after: `&testBlackID` (Go doesn't allow taking the address of a `const`) in a test file, and `ProcessMove(ctx, session, san)` — missing the `color` argument `move.go`'s actual signature requires. Both caught by re-reading source rather than trusting memory of having written it correctly.
- The `edit_file` atomic-failure issue described above under Lessons Learned — concretely, this left the "gRPC client" checkbox in `PHASE_3.md` unchecked for a full turn before being caught and fixed.

### Checklist Progress

- **Step 1** (DB + idempotency): ✅ Complete
- **Step 2** (Proto/gRPC contract): ✅ Complete
- **Step 3** (`internal/matchmaking`): ✅ Complete except Integration Tests, which is 🔄 partial by design — the remaining scenarios (full queue→SSE→connect flow, 409 check) genuinely depend on Step 4 existing
- **Step 4** (`matchmaking-service` deployable): ❌ Not started
- **Step 5** (`ConnectClaimsTTL` config fix): ❌ Not started
- **Step 6** (nginx): ✅ Complete
- **Step 7** (Integration Testing, full): ❌ Not started (partially subsumed by Step 3's own tests)
- **Step 8** (Documentation): 🔄 Partial — `PHASE_3.md` fully current; `ARCHITECTURE.md` **not** updated this session to reflect what's now actually built (still describes decided-but-unimplemented state for everything touched this session); `CLAUDE.md` updated now, in this output

### Technical Debt Introduced

- **TD-P3-005** (new): `Manager.CreateMatchedGame`'s idempotent-retry path does not re-arm `matchedOpponentConnectTimeout` or re-claim ownership, on the assumption neither step could plausibly fail independently of the atomic insert itself. Documented in the method's own doc comment at introduction time, not discovered later. | Phase 3 (implementation) | Not scheduled — same accepted-gap shape as TD-P2-006

### Files Modified

**New:**
```
migrations/005_add_matchmaking_request_id.{up,down}.sql
migrations/006_add_games_activated_at.{up,down}.sql
proto/matchmakingv1/matchmaking.proto
proto/matchmakingv1/matchmaking.pb.go          (generated)
proto/matchmakingv1/matchmaking_grpc.pb.go     (generated)
internal/rpc/interceptor.go
internal/rpc/interceptor_test.go
internal/matchmaking/pairing.go
internal/matchmaking/reporter.go
internal/matchmaking/testmain_test.go
internal/matchmaking/pairing_test.go
```

**Modified:**
```
internal/store/models.go
internal/store/game_store.go
internal/store/game_store_test.go
internal/game/session.go
internal/game/session_test.go
internal/game/manager.go
internal/game/move.go
internal/game/resolve.go
internal/game/resolve_test.go
internal/game/directory.go
internal/game/directory_test.go
internal/game/heartbeat.go
cmd/server/main.go
.env.example
nginx.conf
Makefile
claude/claude_web_project/phases/current/PHASE_3.md
```

### Recommended Next Step

**Step 4: stand up `matchmaking-service` as its own deployable.** Start with `cmd/matchmaking-service/main.go` (new binary in the existing module) plus the `docker-compose.yml` entry the nginx block and `PairingLoop`/`grpcMatchReporter` have both been waiting on. `POST /matchmaking/queue` (mint `MatchmakingClaims`, `ZADD ... NX`, ADR-037's two-read check-before-enqueue) is the natural first endpoint, since it's the only one the check-before-enqueue design actually depends on existing before anything else is testable end-to-end. Estimated 3-4 hours for the binary skeleton + first endpoint + its own test scaffolding.

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

**Phase 1 — MVP: ✅ COMPLETE**

**Phase 2 — Horizontal Scaling: ✅ COMPLETE.** Fully implemented, E2E-verified both manually and via `e2e-phase2.sh` (scenarios 0–6), all Acceptance Criteria confirmed. Full detail: `phases/current/PHASE_2.md`.

**Phase 3 — Matchmaking: 🟡 Implementation IN PROGRESS. Steps 1, 2, 6 complete. Step 3 substantially complete.** Pre-planning (Steps 1–2 design, nine ADRs) completed in a prior session; this and the immediately preceding session moved into actual implementation. `TD-P3-004`, the connection-lifecycle question that gated `Manager.CreateMatchedGame`, is **closed** — see ADR-041/ADR-042 below. `internal/matchmaking`'s pairing loop, the gRPC client, and the `matchmaking_active_game` check-before-enqueue marker are all implemented and tested. **Step 4 (`matchmaking-service` itself, a separate deployable) has not been started** — everything built so far on chess-server's side has no live counterpart to talk to yet. Full detail: Phase 3 Implementation Progress Record below.

---

## Phase 1 Completion Record

(Unchanged from prior sessions — all 10 acceptance criteria MET. See `DECISIONS_LOG_PHASE_1.md` for full detail.)

---

## Phase 2 Completion Record

### What shipped

- Redis-backed `RoutingDirectory`/`RedisDirectory`: ownership (`game:{id}→instanceID`, 30s TTL / 10s renewal) and liveness (`instance_alive:{id}`, 10s TTL / 3s renewal), decoupled per ADR-023. `ClaimOwnership` is a single Lua-script CAS unifying fresh-claim and dead-owner-takeover.
- `GameRegistry.GetOrHydrate`: singleflight-coalesced hydrate-on-miss, detached-context hydration (ADR-027).
- `ConnectClaims`/`SignConnectToken`/`VerifyConnectToken` in `internal/auth`, 10s TTL, alongside unchanged long-lived `PlayerClaims`.
- `GET /games/:id/resolve`: resolves (or claims/hydrates) the owning instance, mints a short-lived `ConnectClaims`, returns a masked `/connect/{instanceLabel}` URL.
- Per-instance heartbeat ticker: batched ownership renewal + liveness renewal, one loop, every 3s. Graceful-shutdown release.
- nginx Edge Proxy: round-robin REST, static `map`-based `/connect/{instanceLabel}` dereference — mechanical only, no Redis/DB access at the proxy layer.
- `WSHandler` fully rewritten for `ConnectClaims` at `/connect/{instanceLabel}` — old `/ws/game/{id}?token=` route removed entirely, not kept in parallel.
- One-shot pre-deploy `migrate` service + `SKIP_MIGRATIONS` flag — TD-008 closed.
- **`JoinGame` is a pure DB operation** (ADR-028) — no `GameRegistry`/`GameSession` access at all.
- **`CreateGame` claims Redis ownership immediately** after local registration (ADR-028), best-effort/non-fatal.
- **New `ABORTED` game status** (ADR-029, migration 004): opponent never joined at all → void, no outcome. Closes TD-P2-005.
- **Persisted disconnect timestamps** (`white_disconnected_at`/`black_disconnected_at`, migration 004, ADR-030).
- **`effectiveDisconnectedAt` fallback** (ADR-031): when neither timestamp was ever written, assume disconnected as of the hydration moment.
- `e2e-phase2.sh`: full scripted E2E harness.

### Architectural Decisions (Phase 2, `DECISIONS_LOG_PHASE_2.md`)

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-021 | Cross-instance architecture | Co-located sessions + Redis ownership/liveness directory |
| ADR-022 | Connection flow | Resolve-then-connect, two-token split |
| ADR-023 | Redis key design | Two separate keys (ownership vs. liveness) |
| ADR-024 | Startup restore | Drop eager `RestoreActiveGames`; `GetOrHydrate` is the single mechanism |
| ADR-025 | TD-008 resolution | One-shot pre-deploy `migrate` service, `SKIP_MIGRATIONS` flag |
| ADR-026 | Redis client library | `github.com/redis/go-redis/v9` |
| ADR-027 | `GetOrHydrate` context handling | Detached from triggering caller, independently bounded |
| ADR-028 | `JoinGame`/`CreateGame` cross-instance correction | `JoinGame` pure DB operation; `CreateGame` claims ownership immediately |
| ADR-029 | `ABORTED` status | New terminal status for "opponent never joined at all" |
| ADR-030 | Disconnect-timestamp persistence | Postgres columns, not Redis |
| ADR-031 | `NULL`-ambiguity fix | `effectiveDisconnectedAt`: assume disconnected as of hydration time |

### Critical bugs found via real E2E testing (all closed)

See prior CLAUDE.md revisions for full detail — `JoinGame` cross-instance failure + non-atomicity, `CreateGame` missing ownership claim, abandonment semantics wrong for opponent-never-joined and crash-while-both-connected. All closed via ADR-028/029/030/031.

---

## Phase 3 Pre-Planning Completion Record

(Unchanged from prior session — see below for what's changed since. Full design reasoning: `phases/current/PHASE_3_DESIGN_NOTES.md`. Formal decisions: `DECISIONS_LOG_PHASE_3.md` ADR-032–040.)

- **Topology: separate `matchmaking-service`, own container**, not a goroutine inside chess-server (ADR-032).
- **Pairing model: server-picks** — chess-server instances contend directly via `ZPOPMIN` (ADR-032).
- **Match-report RPC**: unary, two methods, shared-secret gRPC interceptor (ADR-033).
- **Re-enqueue**: chess-server owns it directly, gated on `matchmaking_request_id` idempotency (ADR-034).
- **matchmaking-service shares the existing nginx edge proxy's single public origin** (ADR-035).
- **SSE notification**: `MatchmakingClaims{UserID}` JWT, four endpoints (ADR-036, ADR-038).
- **Check-before-enqueue**: `ZSCORE` on the queue ZSET + a new heartbeat-leased `matchmaking_active_game:{userID}` key (ADR-037).
- **ADR-039**: correction to ADR-034's cited rationale (UUID v4 decision unaffected; `games.id` really is v7, not v4 as originally cited).
- **ADR-040**: in-process pub/sub alternative, documented as considered-and-rejected.

### Architectural Decisions (Phase 3, `DECISIONS_LOG_PHASE_3.md`)

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-032 | Matchmaking topology & pairing model | Separate `matchmaking-service`, server-picks |
| ADR-033 | Match-report RPC contract & trust boundary | Unary, two methods, shared-secret interceptor |
| ADR-034 | Re-enqueue ownership | chess-server direct, `matchmaking_request_id` idempotency (rationale corrected by ADR-039) |
| ADR-035 | matchmaking-service network origin | Shared nginx edge proxy |
| ADR-036 | SSE notification design | `MatchmakingClaims`, four endpoints, queue-starvation sweep |
| ADR-037 | Check-before-enqueue mechanism | Heartbeat-renewed Redis marker + existing queue ZSET |
| ADR-038 | Voluntary queue cancellation | `DELETE /matchmaking/queue` |
| ADR-039 | Correction to ADR-034's rationale | UUID v4 decision stands; codebase-wide-convention justification retracted |
| ADR-040 | In-process pub/sub — considered and rejected | Documented deliberately, not adopted |
| **ADR-041** | **First-move grace period** | **New `ACTIVE→ABORTED` edge, move-count-gated timer (20s), universal scope — retroactive Phase 1/2 behavior change, confirmed with user before implementing. Closes the "game reaches ACTIVE but nobody ever moves" gap.** |
| **ADR-042** | **Matched-opponent-never-connects timeout** | **Per-process timer armed synchronously at `Manager.CreateMatchedGame` (60s, `matchedOpponentConnectTimeout`), cancelled on opponent connect. Closes TD-P3-004.** |

---

## Phase 3 Implementation Progress Record

### What shipped this session (and the one immediately preceding it)

- **Step 1 — DB + idempotent game creation**: migration 005 (`matchmaking_request_id`), `GameStore.CreateMatchedGame` (atomic `ON CONFLICT DO NOTHING` insert), `GameStore.GetGameByMatchmakingRequestID`.
- **Step 2 — gRPC contract + nginx**: `proto/matchmakingv1/matchmaking.proto` and its generated stubs (codegen run by user), `internal/rpc`'s shared-secret interceptor (client + server), `nginx.conf`'s `/matchmaking` location block (resolver-based dynamic `proxy_pass`, since matchmaking-service has no `docker-compose.yml` entry yet).
- **ADR-041/042, closing TD-P3-004**: migration 006 (`games.activated_at`), `GameStore.ActivateGame`, `session.go`'s new `ACTIVE→ABORTED` edge, `manager.go`'s generalized timer plumbing (`armTimer`/`cancelTimer` shared by three timer kinds), `onFirstMoveTimeout`/`onOpponentNeverConnectedTimeout`/`onMovePersisted`, `move.go`'s `MoveProcessor.onMovePersisted` hook, and `Manager.CreateMatchedGame` itself (including its idempotent-retry path). Retroactive Phase 1/2 test fixes: `session_test.go` (state-machine edge test), `resolve_test.go` (two abandon-timer-resume tests updated to play moves before simulating a crash, since ADR-041 gates the mechanism they test behind move count ≥ 2).
- **Step 3 — `internal/matchmaking` package**: `pairing.go` (`PairingLoop`, `MatchReporter` interface, bounded-retry `pairAndReport`, `reenqueue`), `reporter.go` (`grpcMatchReporter`, the concrete `MatchReporter`), full integration test suite including `TestPairingLoop_ConcurrentTicks_NoDoubleMatch` — two real `Manager`s racing `tick()` concurrently, the phase's actual learning objective under direct test.
- **`matchmaking_active_game:{userID}` marker (ADR-037)**: `directory.go`'s `ActiveGameMarker`/`SetActiveGameMarker`/`RenewActiveGameMarkersBatch`, `heartbeat.go`'s `renewActiveGameMarkers` (re-signs a fresh token per player per tick rather than storing tokens on `GameSession` — a real structural finding: `JoinGame` never touches a live session cross-instance, so a session-held-token design can't populate Black's token), eager writes at `CreateGame`/`JoinGame`/`CreateMatchedGame`.
- **`cmd/server/main.go`**: optional `PairingLoop`+`grpcMatchReporter` wiring, entirely gated on `MATCHMAKING_SERVICE_ADDR` — every existing deployment unaffected by default.

All verified via `go build`, `go vet`, `go test`, `go test -race`, `go test -tags integration -race -p 1`, `gofmt -l` — all clean.

### What has NOT shipped yet

- **Step 4**: `matchmaking-service` itself does not exist as a binary, container, or `docker-compose.yml` entry. `PairingLoop` and `grpcMatchReporter` are built and tested against a fake/interface, but have never talked to a real server.
- **Step 5**: `ConnectClaimsTTL`/`MatchmakingClaimsTTL` env-configurability — still hardcoded, doc-comment drift still present.
- **Step 7**: full end-to-end integration testing (queue → SSE → connect, the 409 check) — blocked on Step 4.
- **`ARCHITECTURE.md`**: not updated this session to reflect what's now actually built — still describes Phase 3 as decided-but-unimplemented for everything touched here. Needs a reconciliation pass before it can be trusted again as "current state," same discipline already applied to `PHASE_3.md` throughout.

---

## Non-Negotiable Constraints

Unchanged from prior sessions (1–16), still locked. See prior CLAUDE.md revisions for the full list.

17. **A tool call's success must be verified, not assumed from the absence of an error — especially for multi-part or atomic operations.** This session, a single `edit_file` call containing two edits failed entirely (both-or-nothing) when the second edit's `oldText` didn't match — the first edit, which would have matched fine on its own, silently did not apply either. This wasn't caught until an unrelated later re-read of the same file. Any multi-edit tool call whose full resulting diff isn't shown back should be re-verified by re-reading the file, not trusted by default — this is a general instance of Constraint 16's "output requires independent re-verification" principle, applied to tool mechanics rather than review conclusions.

---

## Known Sharp Edges

(All prior sharp edges from earlier sessions remain valid — see prior CLAUDE.md revisions for the full list.)

**New this session:**

- **`edit_file`'s atomic multi-edit failure mode (Constraint 17 above)** — a call with N edits where even one `oldText` fails to match rejects the entire call, including edits that would have matched independently. Concretely bit this session on a `PHASE_3.md` checklist update (the gRPC-client checkbox silently stayed unchecked for a full turn). Always re-read the file after a multi-edit call to confirm, don't infer success from lack of a thrown error alone for the whole call.
- **The Filesystem MCP server can time out mid-session with no warning** — two consecutive 4-minute timeouts reading `resolve.go` this session, requiring a pause and a user-side restart. No data was lost (nothing had been written mid-outage), but any long session should treat this as a real, not hypothetical, possibility and pause cleanly rather than retry-loop against a dead connection.
- **`Manager.CreateMatchedGame`'s exact signature is `(ctx, playerWhiteID, playerBlackID, matchmakingRequestID string) (*GameSession, whiteToken, blackToken string, err error)`; `MoveProcessor.ProcessMove`'s is `(ctx, session, color, san)`, four arguments, not three.** Both were mis-remembered once each this session (a `&testBlackID` address-of-const bug, and a missing `color` argument in a test fix) before being caught by re-reading `move.go`/`manager.go` directly rather than trusting memory of having written them correctly earlier in the same session. Worth treating "I wrote this function ten messages ago, I remember its signature" as exactly the kind of claim Constraint 16 already says needs re-verification, not an exception to it.
- **`JoinGame` never touches a live `GameSession`, ever, by design (ADR-028)** — this constraint from Phase 2 has now visibly rippled into a second, unrelated Phase 3 design (the `matchmaking_active_game` marker's token-sourcing strategy). Worth checking this constraint explicitly, early, for any future feature that wants per-player state at join time, rather than discovering the conflict mid-implementation as happened here.

---

## WebSocket Message Protocol (Phase 2)

Unchanged — see prior CLAUDE.md revisions for full detail (route `/connect/{instanceLabel}?token=<connectToken>`, `ConnectClaims` fields, `CONNECT_TOKEN_EXPIRED` error code, two-step client flow). No protocol-level changes this session.

---

## Key Files and Their Responsibilities (Phase 3 additions, cumulative)

```
migrations/005_add_matchmaking_request_id.{up,down}.sql — matchmaking_request_id idempotency column
migrations/006_add_games_activated_at.{up,down}.sql     — activated_at, ADR-041's hydration anchor

proto/matchmakingv1/matchmaking.proto        — MatchReportService contract
proto/matchmakingv1/matchmaking.pb.go        — generated
proto/matchmakingv1/matchmaking_grpc.pb.go   — generated

internal/rpc/interceptor.go       — shared-secret gRPC interceptor, client + server halves (ADR-033)
internal/rpc/interceptor_test.go  — matches

internal/matchmaking/pairing.go       — PairingLoop, MatchReporter interface, pairAndReport, reenqueue
internal/matchmaking/reporter.go      — grpcMatchReporter, the concrete MatchReporter
internal/matchmaking/testmain_test.go — package-local test scaffolding (own Manager construction helper)
internal/matchmaking/pairing_test.go  — integration tests incl. the concurrent-no-double-match test

internal/game/directory.go   — +ActiveGameMarker, SetActiveGameMarker, RenewActiveGameMarkersBatch (ADR-037)
internal/game/heartbeat.go   — +renewActiveGameMarkers, folded into heartbeatTick
internal/game/session.go     — +ACTIVE→ABORTED edge (ADR-041)
internal/game/manager.go     — +ActivateGame call site, +armTimer/cancelTimer generalized plumbing,
                                +onFirstMoveTimeout/onOpponentNeverConnectedTimeout/onMovePersisted,
                                +Manager.CreateMatchedGame, +eager marker writes (CreateGame/JoinGame/
                                CreateMatchedGame)
internal/game/move.go        — +MoveProcessor.onMovePersisted hook (ADR-041 move-pipeline integration)
internal/game/resolve.go     — armTimersForGame replaces armAbandonTimersForGame at this call site

cmd/server/main.go — +optional PairingLoop/grpcMatchReporter wiring, gated on MATCHMAKING_SERVICE_ADDR
.env.example        — +MATCHMAKING_SERVICE_ADDR/MATCHMAKING_SHARED_SECRET/MATCHMAKING_PAIRING_INTERVAL_MS
nginx.conf           — +/matchmaking location block
Makefile             — +proto/install-proto-tools targets
```

All Phase 1/2 file responsibilities unchanged — see prior CLAUDE.md revisions for the full listing.

**Not yet implemented, still empty**: `cmd/matchmaking-service/` (Step 4 — the deployable itself does not exist yet, though everything on chess-server's side that would talk to it is built and tested).

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
TD-008: CLOSED (Phase 2 Step 9) — one-shot migrate service + SKIP_MIGRATIONS flag. See ADR-025.

TD-P2-001: No fencing token — narrow, TTL-bounded false-positive liveness window | Phase 2 | Fix by: Phase 8 (k8s Lease)
TD-P2-002: Static Edge Proxy config — scaling requires a config edit + reload | Phase 2 | Fix by: Phase 8
TD-P2-003: No active liveness probing — detection is purely TTL-based | Phase 2 | Revisit only if TD-P2-001's window matters
TD-P2-004: A session hydrated for an already-terminal game stays registered indefinitely | Phase 2 | Not scheduled
TD-P2-005: CLOSED (ADR-029) — new ABORTED status
TD-P2-006: A game nobody ever /resolves again after an instance dies is not covered by ADR-030/031's fix | Phase 2 | Not scheduled

CRITICAL-1, CRITICAL-2: CLOSED (ADR-028).

TD-P3-001: FIFO queue only — no ELO-based pairing | Phase 3 (design) | Fix by: Phase 4
TD-P3-002: Single time control (10+0) only | Phase 3 (design) | Fix by: Phase 4
TD-P3-003: matchmaking-service's SSE connections live in an in-memory map, correct only at M=1 | Phase 3 (design) | Revisit if matchmaking-service needs to scale
TD-P3-004: CLOSED (2026-08-10, ADR-041/ADR-042). A matched player whose opponent never connects at all now has a dedicated arm-at-creation timeout. A related but distinct gap (a game reaching ACTIVE but never receiving a first move) was found while resolving this and closed separately by ADR-041's universal-scope first-move grace period. | Resolved — see ADR-041/ADR-042
TD-P3-005: Manager.CreateMatchedGame's idempotent-retry path does not re-arm matchedOpponentConnectTimeout or re-claim ownership, on the assumption neither could plausibly fail independently of the atomic insert itself. Documented at introduction, same accepted-gap shape as TD-P2-006. | Phase 3 (implementation) | Not scheduled
```

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1–12 | 2025-01-XX to 2026-07-10 | Phase 1 complete (see prior CLAUDE.md revisions) |
| 13 | 2026-07-15 | Phase 2 architecture design. ADR-021–025 written. |
| 14 | 2026-07-21 | Phase 2 implementation Steps 1–10 + Step 11 started. ADR-026, ADR-027 logged. Two critical bugs found via real multi-instance E2E, diagnosed but not fixed at session end. |
| 15 | 2026-07-22 to 2026-07-29 | Full codebase re-analysis; implemented ADR-028 (JoinGame/CreateGame fix, correcting a flawed plan from session 14), ADR-029 (ABORTED status), ADR-030/031 (disconnect-timestamp persistence + crash-while-both-connected fix, the latter via an independently-briefed review session). Built `e2e-phase2.sh`. Found and fixed two flaky-test bugs. Full Step 12 documentation pass. |
| 16 | 2026-08-08 to 2026-08-09 | **Phase 3 pre-planning, full cycle.** Discarded a hallucinated prior attempt. Full design pass from scratch: topology, RPC contract, re-enqueue ownership, SSE design, check-before-enqueue. ADR-032–038 written, independently reviewed, reconciled with two rounds of self-caught correction (envelope conformance, pub/sub history-accuracy). TD-P3-004 formalized with an explicit gate on Step 3. Implementation not started. |
| 17 | 2026-08-09 to 2026-08-13 | **Phase 3 implementation, Steps 1–3.** Step 1 (migration 005, `CreateMatchedGame` store method) and Step 2 (proto contract, nginx block) built after session-start ritual and a module-structure decision (single module, resolved by user directly rather than via a new ADR). User closed TD-P3-004 with ADR-041/ADR-042 (drafted in a prior session, verified against source and implemented here): first-move grace period (retroactive Phase 1/2 change, explicitly confirmed with user before implementing — two categories of existing tests needed fixing as a direct, predicted result) and the matched-opponent-never-connects timeout, closing `Manager.CreateMatchedGame`'s gate. Built `internal/matchmaking`'s `pairing.go` (tested against an interface, since generated protobuf stubs didn't exist yet) and, once the user ran codegen, `reporter.go`'s concrete gRPC-backed implementation. Extended the ADR-037 check-before-enqueue marker across `directory.go`/`heartbeat.go`/`manager.go`, discovering a real structural interaction with `JoinGame`'s Phase 2 cross-instance constraint along the way. Wired everything optionally into `cmd/server/main.go`. One Filesystem MCP outage (resumed cleanly), two self-caught implementation bugs (an address-of-constant compile error, a missing function argument), and one silently-failed atomic multi-edit tool call (caught and fixed) — all documented as new sharp edges / a new constraint (17) above. All work verified via user-run `go build`/`vet`/`test`/`test -race`/`test -tags integration -race -p 1`, all clean. `ARCHITECTURE.md` not updated this session — flagged as outstanding. |

---

## Next Session

**Step 4: stand up `matchmaking-service` as its own deployable.** Everything chess-server needs to talk to it — `PairingLoop`, `grpcMatchReporter`, the nginx routing block, the shared-secret interceptor — is built and tested, but has never talked to a real server. Start with `cmd/matchmaking-service/main.go` (new binary, same module) and its `docker-compose.yml` entry, then `POST /matchmaking/queue` first (mint `MatchmakingClaims`, `ZADD ... NX`, ADR-037's two-read check-before-enqueue) — the endpoint the check-before-enqueue design most directly depends on existing before anything else is end-to-end testable. **Also outstanding, lower priority**: `ARCHITECTURE.md` needs a reconciliation pass to reflect Steps 1–3's actual implemented state (currently still describes Phase 3 as decided-but-unbuilt for everything landed this session) — same discipline already applied to `PHASE_3.md` throughout, not yet extended to this document.
```