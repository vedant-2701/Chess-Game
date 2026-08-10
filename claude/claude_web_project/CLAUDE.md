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

**Phase 2 — Horizontal Scaling: ✅ COMPLETE.** Fully implemented, E2E-verified both manually and via `e2e-phase2.sh` (scenarios 0–6), all Acceptance Criteria confirmed. Three rounds of real multi-instance testing surfaced bugs the design-phase audit (ADR-021) did not catch — all fixed, documented as ADR-026 through ADR-031. Full detail: `phases/current/PHASE_2.md`.

**Phase 3 — Matchmaking: 🟡 Pre-planning COMPLETE, independently reviewed, reconciled. Implementation NOT started.** The independent pre-planning audit this file's prior revision called for (comparing `PHASE_3.md` against Phase 2's actual final shape) has run, followed by a full from-scratch design pass covering topology, the match-report RPC contract, re-enqueue semantics, the SSE notification design, and check-before-enqueue — nine ADRs (`DECISIONS_LOG_PHASE_3.md` ADR-032 through ADR-040), independently reviewed in a separately-briefed session per Constraint 15, with two real corrections found and two claims confirmed accurate on direct re-verification. `PHASE_3.md` and `ARCHITECTURE.md` are both reconciled to this state. Full detail: Phase 3 Pre-Planning Completion Record below. **One connection-lifecycle question is deliberately still open** (TD-P3-004) — gated explicitly on `PHASE_3.md`'s Step 3 checklist item, not blocking Steps 1–2.

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
- **`JoinGame` is a pure DB operation** (ADR-028) — no `GameRegistry`/`GameSession` access at all, matching this project's own original Phase 2 design statement, which the initial implementation had violated.
- **`CreateGame` claims Redis ownership immediately** after local registration (ADR-028), best-effort/non-fatal.
- **New `ABORTED` game status** (ADR-029, migration 004): opponent never joined at all → void, no outcome, no outcome_reason — distinct from `ABANDONED` (requires the game to have actually reached `ACTIVE`). Closes TD-P2-005.
- **Persisted disconnect timestamps** (`white_disconnected_at`/`black_disconnected_at`, migration 004, ADR-030): survive an instance dying mid-grace-period so a surviving instance resumes the correct remaining duration on hydrate, chosen over Redis specifically because Redis's outage is an explicitly *tolerated* failure mode in this design while abandonment fairness shouldn't degrade silently when that happens.
- **`effectiveDisconnectedAt` fallback** (ADR-031): when neither timestamp was ever written — the instance died before either individual disconnect could be observed, e.g. a crash while both players were still connected — assume disconnected as of the hydration moment rather than assuming fine. A player who is in fact still connected self-cancels the resulting defensive timer via their own reconnect.
- `e2e-phase2.sh`: full scripted E2E harness (state-persisted across invocations, persistent FIFO-backed WebSocket connections, guided scenarios 0–6, `regress N` stress command). Companion/successor to `get-id-and-token.sh` and `phase2_step11_e2e_walkthrough.md`.

### Architectural Decisions (Phase 2, `DECISIONS_LOG_PHASE_2.md`)

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-021 | Cross-instance architecture | Co-located sessions + Redis ownership/liveness directory, not cross-instance state sync |
| ADR-022 | Connection flow | Resolve-then-connect, two-token split (`PlayerClaims` + `ConnectClaims`), not connect-then-relay |
| ADR-023 | Redis key design | Two separate keys (ownership vs. liveness), not one combined key or active HTTP probing |
| ADR-024 | Startup restore | Drop eager `RestoreActiveGames`; `GetOrHydrate` is the single mechanism for local registry misses |
| ADR-025 | TD-008 resolution | One-shot pre-deploy `migrate` service, `SKIP_MIGRATIONS` flag |
| ADR-026 | Redis client library | `github.com/redis/go-redis/v9` |
| ADR-027 | `GetOrHydrate` context handling | Detached from triggering caller, independently bounded (generalizes ADR-019) |
| ADR-028 | `JoinGame`/`CreateGame` cross-instance correction | `JoinGame` made a pure DB operation (not given a `GetOrHydrate` fallback — that would have reintroduced split-brain risk on a code path with no ownership-claim precondition); `CreateGame` claims Redis ownership immediately |
| ADR-029 | `ABORTED` status | New terminal status for "opponent never joined at all" — void, not a phantom win/draw |
| ADR-030 | Disconnect-timestamp persistence | Postgres columns on `games`, not Redis — failure-mode alignment (Redis outage is tolerated for routing, not acceptable for a fairness-determining fact) |
| ADR-031 | `NULL`-ambiguity fix | `effectiveDisconnectedAt`: assume disconnected as of hydration time when no timestamp exists for a non-terminal game, closing the crash-while-both-connected gap ADR-030 alone didn't cover |

### Critical bugs found via real E2E testing (all closed)

Three separate rounds of multi-instance testing against the real Docker cluster surfaced bugs the design-phase audit didn't catch — **none were reachable by single-`Manager` integration tests**, since one `Manager` instance structurally cannot reproduce what nginx's round-robin does across genuinely separate processes. This is now a standing testing discipline for this project (see Non-Negotiable Constraints below).

1. **`JoinGame` cross-instance failure + a worse non-atomicity bug.** `JoinGame` touched `GameRegistry` directly; under round-robin, ~50% of joins landed on an instance that never created the game and failed. Worse: the DB write (`UpdatePlayerBlack`) committed *before* the registry check, so a failed join could leave a game permanently unjoinable — DB says joined, client got a 500, no token ever issued. Root cause was an architecture violation, not a missing fallback: `JoinGame` should never have touched the registry at all (this document's own `PHASE_2.md` already said so). Fixed (ADR-028) by removing the registry access entirely — also eliminated the non-atomicity bug as a free side effect, since nothing remains after the DB write that can fail.
2. **`CreateGame` never claimed Redis ownership**, causing indefinite `"lost ownership renewal"` heartbeat log spam for any created-but-never-resolved game. Fixed (ADR-028) with a best-effort `ClaimOwnership` call right after local registration.
3. **Abandonment semantics were wrong for "opponent never joined," and didn't survive instance failover for "both players connected, instance dies."** Two related but distinct fixes: ADR-029 (`ABORTED` status, closes TD-P2-005) and ADR-030/031 (persisted disconnect timestamps + the crash-while-both-connected fallback). The second gap (ADR-031) was found by an *independently-briefed* review session specifically instructed not to trust the first session's own design as correct — see "Non-Negotiable Constraints" below for why that pattern is now standing practice.

### Two genuinely flaky test bugs found and fixed this session (not production bugs)

- **JWT `"tampered signature"` test cases** (`internal/auth/token_test.go`, both `TestVerifyPlayerToken` and `TestVerifyConnectToken`): corrupted only the *last* character of the token. Base64's final character of a 32-byte HMAC-SHA256 signature carries 2 "don't-care" padding bits the decoder ignores — a fixed last-character replacement collides with the original's decoded bytes on ~1 in 16 runs (the signature varies every run since `IssuedAt` feeds into it), making the test silently pass-when-it-should-fail that often. Fixed by flipping a *middle* character of the signature segment instead — the pattern the file's own `"tampered payload"` case already used correctly, just not applied to the sibling case.
- **Two of the ADR-030/031 regression tests, when first written, had test-setup bugs**, not production issues: (a) `session.Transition(ACTIVE)` only flips in-memory state, never the DB row — `onAbandonTimeout`'s CAS-based DB write (ADR-016 pattern) correctly rejected a write against a row that wasn't really `ACTIVE`, exposing that the test forgot to sync the DB too; (b) comparing a full-nanosecond-precision `time.Now()`-derived value against one that round-tripped through Postgres's microsecond-precision `TIMESTAMPTZ` — `.Truncate(time.Microsecond)` before writing fixed it. Both are now flagged in Known Sharp Edges below since they'll bite again.

---

## Phase 3 Pre-Planning Completion Record

### What was decided

Full design reasoning: `phases/current/PHASE_3_DESIGN_NOTES.md` (16 sections). Formal decisions: `DECISIONS_LOG_PHASE_3.md` ADR-032–040. Reconciled checklist/contract: `phases/current/PHASE_3.md`.

- **Topology: separate `matchmaking-service`, own container**, not a goroutine inside chess-server — the deciding factor was SSE match-notification delivery, not the matchmaking logic itself. A player's SSE connection is sticky to whichever process accepts it; an in-process design leaves chess-server (built around single-owner, no-fan-out state per ADR-021) with no clean place to absorb the resulting N×N delivery problem across instances. Extraction collapses this to at most M×M within matchmaking-service's own replica set (ADR-032).
- **Pairing model: server-picks.** chess-server instances contend directly for pairs against a shared Redis queue via `ZPOPMIN`'s atomicity — matchmaking-service never assigns, round-robins, or brokers a race between instances; it only relays outcomes after the fact (ADR-032).
- **Match-report RPC**: unary, two methods (`ReportMatchCreated`/`ReportMatchmakingFailed`), shared-secret gRPC interceptor (Docker Compose has no `NetworkPolicy` equivalent — deliberate interim choice, mTLS post-Phase-3) (ADR-033).
- **Re-enqueue**: chess-server owns it directly (one `ZADD`, no RPC for the recoverable case), gated on a new `matchmaking_request_id UUID UNIQUE` column closing an ACK-loss double-booking hazard the re-enqueue-ownership question surfaced (ADR-034).
- **matchmaking-service shares the existing nginx edge proxy's single public origin** — no new `connect_url` field anywhere in this design; `instance_label` alone stays sufficient (ADR-035).
- **SSE notification**: new `MatchmakingClaims{UserID}` JWT (same signing secret, 60s TTL mirroring `ConnectClaimsTTL`'s *intended* value — see Known Sharp Edges), `POST /matchmaking/queue` (idempotent via `ZADD NX`), `GET /matchmaking/stream`, `GET /matchmaking/status` (polling backstop, doubles as the SSE-fallback), `DELETE /matchmaking/queue` (voluntary cancel), queue-timeout sweep (ADR-036, ADR-038).
- **Check-before-enqueue**: two independently-sourced Redis facts, not one merged status key — queue membership via the existing queue ZSET directly (`ZSCORE`, zero new writes), and active-game status via a new `matchmaking_active_game:{userID}` key, written/renewed *only* by chess-server (heartbeat-leased, `OwnershipTTL`/`OwnershipRenewInterval` reused, not new numbers), read-only for matchmaking-service. Applies to every locally-owned active game regardless of origin — a player mid-game via a shared link is blocked from matchmaking too, by explicit product decision (ADR-037).

### Independent review findings (ADR-039, ADR-040)

Per Constraint 15, a separately-briefed session reviewed ADR-032–038 against the actual codebase before any of this was treated as final. Confirmed accurate on direct re-verification (not just trusted from the review): `games.id` really is UUID v7 (`manager.go`'s `CreateGame` calls `uuid.NewV7()` explicitly) and `ConnectClaimsTTL` really is `60 * time.Second` against a doc comment claiming "(10s)" — both real, independently confirmed findings, not assumed from the review's say-so. Genuinely wrong and corrected: ADR-034's original rationale cited a stale `game_store.go` doc comment ("game.ID (UUID v4)") as evidence of a codebase-wide v4 convention; the actual generating call site uses v7. The decision itself (v4 for `matchmaking_request_id`) was unaffected — that column is a correlation/idempotency token checked by exact match, never range-scanned, so v7's specific benefit never applied to it regardless — only the cited justification was wrong. Corrected via ADR-039, per this project's append-only discipline (new entry, ADR-034's original text left untouched, same pattern as ADR-031 correcting ADR-030).

Separately, ADR-040 documents an in-process-plus-Redis-pub/sub alternative as considered-and-rejected — deliberately, not because it surfaced during the actual design pass (it didn't), but because it's the exact mechanism a prior, fully-discarded Phase 3 attempt (hallucinated content, discarded 2026-08-06, see below) used. Logged specifically so a future session finding this idea again has an honest, substantive rejection on record (no delivery guarantee, unlike the chosen design's built-in retry/dedup/backstop) rather than mistaking it for either untested contamination or a genuinely novel idea nobody has considered.

### A second, self-caught round of errors, found during reconciliation itself

The reconciliation pass that incorporated the independent review's findings into `PHASE_3.md`/`ARCHITECTURE.md` was itself re-verified rather than trusted — and that re-verification found two further problems, both fixed in the same pass:

- **A genuine bug**: `PHASE_3.md`'s endpoint examples didn't follow `CODING_GUIDELINES.md` §7's envelope (`internal/api/response.go`'s actual `writeData`/`writeError`) — the 409 response showed a bare error *string*, dropping the required error code entirely. Fixed with a named error code (`ALREADY_IN_ACTIVE_GAME`) and an `existingGame` field carrying connect info without breaking the "no client special-case" property ADR-037 was built for. This traced back through three documents (`PHASE_3_DESIGN_NOTES.md` §15 had the same gap, since it was written first) — all three fixed, not just the one first noticed. Extended afterward to `ARCHITECTURE.md`'s older, already-shipped Phase 1/2 endpoint examples too, on explicit user confirmation, for full project-wide consistency rather than leaving Phase 3 as the only correctly-documented part.
- **A design-history integrity check**: `PHASE_3.md`'s claim that the pub/sub alternative was "evaluated and rejected during design" was challenged directly, since the session doing the reconciling had first-hand knowledge the actual design pass never considered it — this turned out to be a deliberate, disclosed choice (documented via ADR-040 as described above, tense corrected to reflect it was added afterward, not recovered from history), not contamination, but it was verified as such rather than assumed either way.

This is offered as a concrete instance of Constraint 15 generalizing one level further than originally stated: **a review session's own output still needs independent re-verification before being trusted — reviewing something once does not make its conclusions self-certifying, even when the review was itself careful and largely correct.** See Constraint 16 below.

### Architectural Decisions (Phase 3, `DECISIONS_LOG_PHASE_3.md`)

| ID | Decision | Chosen |
|----|----------|--------|
| ADR-032 | Matchmaking topology & pairing model | Separate `matchmaking-service`, server-picks (not in-process, not service-assigns) |
| ADR-033 | Match-report RPC contract & trust boundary | Unary, two methods, enum failure reason, shared-secret interceptor |
| ADR-034 | Re-enqueue ownership | chess-server direct, `matchmaking_request_id` idempotency (rationale corrected by ADR-039) |
| ADR-035 | matchmaking-service network origin | Shared nginx edge proxy, no `connect_url` field |
| ADR-036 | SSE notification design | `MatchmakingClaims`, four endpoints, queue-starvation sweep |
| ADR-037 | Check-before-enqueue mechanism | Heartbeat-renewed Redis marker + existing queue ZSET, not a DB query or explicit-clear cache |
| ADR-038 | Voluntary queue cancellation | `DELETE /matchmaking/queue` |
| ADR-039 | Correction to ADR-034's rationale | UUID v4 decision stands; codebase-wide-convention justification retracted (`games.id` is v7) |
| ADR-040 | In-process pub/sub — considered and rejected | Documented deliberately, not adopted; ADR-032's decision unaffected |

---

## Non-Negotiable Constraints

Unchanged from prior sessions (1–13), still locked. See prior CLAUDE.md revisions for the full list. Notably still governing this session's work:

12. **A context's cancellation timing chosen for one operation must be re-verified for every other operation later wired through the same context — never assumed safe by inheritance.** (ADR-019) Invoked again this session for `HandleDisconnect`'s disconnect-timestamp persist write (ADR-030) — correctly detached, same pattern as the existing clock-persist write.

13. **Any fix applied to one code path handling a shared cross-cutting concern must be checked against every other code path with the same structural dependency, not assumed to be the only place it's needed.** (Formalized this session from prior session's observation about `HandleConnect`'s `GetOrHydrate` fix vs. `JoinGame`'s missed identical need — see Critical bugs #1 above, which is exactly this pattern recurring.)

14. **Single-`Manager`-instance tests cannot validate cross-instance behavior, full stop — this is now a standing testing requirement, not a one-off lesson.** Any test asserting behavior that's supposed to work "across instances" (routing, ownership handoff, registry-miss recovery, abandonment-timer resumption) must construct **two separate `Manager` instances** sharing the same `testPool`/`testRedisClient` (`newTestManagerWithDirectory`, `internal/game/resolve_test.go`). This is not optional polish — every one of Phase 2's three critical-bug rounds this session was invisible to every existing single-`Manager` test suite and only surfaced via the real Docker cluster.

15. **A design's own author is a bad auditor of that same design under time pressure — for genuinely consequential cross-cutting fixes (not routine bugs), get an independently-briefed second pass before implementing, not after.** This session's ADR-030 (persisted disconnect timestamps) shipped with a real, non-trivial gap (the crash-while-both-connected case) that survived one full round of "explain the design back to the user" and direct source-reading. It was caught by deliberately handing the problem to a *separate, neutrally-briefed* review — explicitly instructed not to defer to the first session's own design — rather than by the same session re-checking its own work. When a fix is this consequential (touches failure-mode correctness, not a routine CRUD bug), prefer this pattern proactively rather than discovering the need for it after the fact.

16. **A review session's conclusions require the same independent re-verification as the design they're reviewing — trust neither by default.** (Phase 3 pre-planning) The independent review of ADR-032–038 found real, correct issues (ADR-034's stale citation) and confirmed other claims accurately — but the reconciliation session that incorporated those findings still re-verified both categories directly against source rather than accepting the review's word, and separately caught a claim (pub/sub "evaluated during design") that needed challenging on the reconciler's own first-hand knowledge of the actual design history. Constraint 15 established that a design's author is a poor auditor of their own work; this extends it — a reviewer's output is not self-certifying either, one level of review does not obviate the next.

---

## Known Sharp Edges

(All prior sharp edges from earlier sessions remain valid — see prior CLAUDE.md revisions for the full list: migrate CLI URL scheme, `notnil/chess` panics, `ReadLoop` context lifetime, `HandleDisconnect`'s detached clock-persist context, `CloseConnections` ordering requirement, goleak `IgnoreCurrent()` pattern, `gopls` unreliability on filesystem-MCP writes and integration-tagged files, Redis client test isolation via DB 1, `Connection.Close()`'s nil-`wsConn` panic risk, `ConnectClaimsTTL` timing for manual testing, cross-instance bugs needing two-`Manager` tests, `session.Transition` in-memory-only, Postgres `TIMESTAMPTZ` microsecond truncation, `Manager.abandonTimers` per-process-only, `Filesystem` MCP vs. sandbox tool distinction, `edit_file` exact-match requirements, `go test -run` non-OR behavior, `e2e-phase2.sh`'s `ws_disconnect` latency discrepancy, etc.)

**New this session:**

- **The `Filesystem:*` vs. sandbox `str_replace`/`create_file` tool confusion (already documented as a sharp edge from a prior session) recurred this session** — mid-conversation, attempting to edit `PHASE_3_DESIGN_NOTES.md` via the plain `str_replace` tool instead of `Filesystem:edit_file` produced a clean "file not found" error (the sandbox correctly has no copy of a file that only exists on the real WSL disk) rather than a silent wrong-location write — caught immediately, no data lost, but worth noting the existing documentation of this edge did not prevent it recurring. Treat as a standing risk to check for explicitly before any file-edit tool call, not just something to remember once.
- **`ConnectClaimsTTL` is `60 * time.Second` in `internal/auth/token.go`, against a doc comment claiming "(10s)"** — confirmed directly this session, not inherited from any prior session's notes. Real drift between comment and code, most likely the constant was changed locally for manual-testing convenience at some point and never reverted. `PHASE_3.md` Step 5 tracks making both `ConnectClaimsTTL` and the new `MatchmakingClaimsTTL` env-configurable with a 10s default (matching the comment's evident original intent) as an implementation task — not fixed yet, only documented.
- **`game_store.go`'s `CreateGame` doc comment ("game.ID (UUID v4)") is stale relative to the actual generating call site** — `manager.go`'s `CreateGame` uses `uuid.NewV7()` explicitly, with its own comment stating the B-tree-locality rationale. `games.id` is v7. This is a second, independent instance of the same failure mode already logged under this section from Phase 2 (a doc comment describing an expectation, not the code that actually implements it) — worth treating as a *pattern* in this codebase specifically (stale comments on `store`-layer methods describing preconditions set by `Manager`-layer call sites), not a one-off. Not fixed yet (source-code fix, not a doc-reconciliation task) — flagged for an implementation session.
- **`CODING_GUIDELINES.md` §7's envelope rule (`{"data": ...}` / `{"error": {"code","message"}}`) is the real, code-enforced contract (`internal/api/response.go`'s `writeData`/`writeError`, `GET /health` the sole exception) — but this project's own documentation of endpoints, including already-shipped Phase 1/2 ones in `ARCHITECTURE.md`, did not consistently show it, appearing unwrapped instead.** Fixed this session in `PHASE_3.md`, `PHASE_3_DESIGN_NOTES.md`, and `ARCHITECTURE.md`'s existing `/games` family. Worth checking any *future* endpoint documentation against `response.go` directly rather than pattern-matching off how nearby examples happen to be shown, since that pattern itself was found to be wrong more than once.

---

## WebSocket Message Protocol (Phase 2)

Unchanged from the prior session's summary — see that section's content if a future session needs the full detail (route `/connect/{instanceLabel}?token=<connectToken>`, `ConnectClaims` fields, `CONNECT_TOKEN_EXPIRED` error code, two-step client flow). No protocol-level changes this session — Phase 3 pre-planning changes no shipped wire format; ADR-032–040 govern a not-yet-implemented system.

---

## Key Files and Their Responsibilities (Phase 2 additions, cumulative)

```
internal/game/directory.go       — NewRedisClient, RoutingDirectory interface, RedisDirectory impl
internal/game/directory_test.go  — concurrent-claim race tests, CAS behavior tests
internal/game/resolve.go         — Manager.ResolveGame, hydrateGameSession, armAbandonTimersForGame call site
internal/game/resolve_test.go    — ResolveGame tests incl. terminal-game hydration, goleak check,
                                    two-Manager abandon-timer-resumption regression tests (ADR-030/031)
internal/game/heartbeat.go       — Manager.StartHeartbeat, heartbeatTick, releaseHeartbeatEntries
internal/game/heartbeat_test.go  — heartbeat lifecycle tests incl. goleak check
internal/game/handleconnect_hydrate_test.go — HandleConnect's GetOrHydrate fallback, direct test
internal/game/abandon_test.go    — NEW, no build tag: pure-function tests for
                                    remainingAbandonDuration/effectiveDisconnectedAt (ADR-030/031) —
                                    exists specifically because *time.Timer exposes no remaining-duration
                                    accessor, so the underlying math had to be pulled out to be testable
                                    without sleeping or making abandonTimeout a var
internal/api/resolve_test.go     — HTTP-level resolve endpoint tests
internal/api/ws_handler.go       — ConnectClaims/instanceLabel (Step 8)
internal/api/ws_handler_test.go  — matches
migrations/004_add_aborted_and_disconnect_tracking.{up,down}.sql — NEW, ABORTED status +
                                    white/black_disconnected_at columns (ADR-029/030)
Dockerfile, nginx.conf            — Edge Proxy
e2e-phase2.sh                     — NEW, full scripted E2E harness (state persistence, FIFO-backed
                                    WebSocket connections via wscat, guided scenario0–scenario6,
                                    regress N stress command, log capture). Companion to
                                    phase2_step11_e2e_walkthrough.md, which also gained a Scenario 6
                                    section this session.
```

All other Phase 1 file responsibilities unchanged — see prior CLAUDE.md revisions for the full listing.

**Phase 3 has not been implemented — no new files exist in the repository yet.** Planned files (`internal/matchmaking/`, `proto/`, a new `matchmaking-service/` deployable, migration for `matchmaking_request_id`) are enumerated in full in `phases/current/PHASE_3.md`'s Implementation Checklist, not duplicated here until they actually exist.

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

TD-P2-001: No fencing token — narrow, TTL-bounded false-positive liveness window can theoretically split a live game. Addendum (ADR-031): the same window can now also spuriously abandon-lose a genuinely-connected player, not just cause duplicate processing. | Phase 2 | Fix by: Phase 8 (k8s Lease)
TD-P2-002: Static Edge Proxy config — scaling requires a config edit + reload | Phase 2 | Fix by: Phase 8 (dynamic service discovery)
TD-P2-003: No active liveness probing — detection is purely TTL-based | Phase 2 | Revisit only if TD-P2-001's window is shown to matter
TD-P2-004: A session hydrated via resolve for an already-terminal game stays registered in the local GameRegistry indefinitely — nothing currently evicts it | Phase 2 Step 5 | Not scheduled; revisit if shown to matter
TD-P2-005: onAbandonTimeout's both-disconnected branch could never succeed for a WAITING_FOR_PLAYER game (no WAITING→ABANDONED edge) | CLOSED (ADR-029) — new ABORTED status closes the gap correctly rather than patching the edge onto the old both-disconnected/DRAW branch
TD-P2-006: A game where nobody ever calls /resolve again after an instance dies is not covered by ADR-030/031's fix, and cannot be by construction — nothing runs if nobody triggers a hydrate | Phase 2 (ADR-031) | Not scheduled; candidate design is a periodic sweep folded into the existing heartbeat tick, not built speculatively

CRITICAL-1, CRITICAL-2: CLOSED (ADR-028) — see Phase 2 Completion Record above.

TD-P3-001: FIFO queue only — no ELO-based pairing | Phase 3 (design) | Fix by: Phase 4 (ELO exists then)
TD-P3-002: Single time control (10+0) only, matching TD-005 | Phase 3 (design) | Fix by: Phase 4
TD-P3-003: matchmaking-service's SSE connections live in an in-memory map, correct only at one replica (M=1). Scaling matchmaking-service itself beyond one replica reintroduces the same delivery-affinity problem Phase 3 extracted matchmaking out of chess-server to avoid, at a smaller scale (ADR-036). | Phase 3 (design) | Revisit if/when matchmaking-service needs to scale
TD-P3-004: A matched player who connects and stays connected while their assigned opponent never connects at all has no abandonment mechanism — confirmed by tracing HandleConnect/HandleDisconnect/onAbandonTimeout directly: no disconnect event ever fires for either color in this scenario (the connected player never disconnects; the absent one never connected to disconnect from), so no timer of any kind arms, and the game sits in WAITING_FOR_PLAYER indefinitely. Structurally different from the already-resolved half of this same question (a matched player who connects, then also disconnects, while the opponent never showed — confirmed correctly handled by the existing WAITING→ABORTED path, ADR-029, no new code needed). Deliberately undesigned: entangled with HandleConnect/HandleDisconnect, ADR-030/031's failover grace-period persistence, and the existing abandonment timer machinery closely enough to need its own dedicated session, not a bolt-on fix to one path in isolation. Explicit gate placed directly on PHASE_3.md Step 3's CreateMatchedGame checklist item. | Phase 3 (design) | Before or during Step 3 — user has explicitly confirmed this will be picked up in a dedicated session, deliberately deferred, not forgotten
```

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1–12 | 2025-01-XX to 2026-07-10 | Phase 1 complete (see prior CLAUDE.md revisions for full detail) |
| 13 | 2026-07-15 | Phase 2 architecture design: audited original Redis pub/sub plan, found critical bugs, rejected ID-prefix and consistent-hash-ring alternatives, arrived at co-located sessions + Redis routing directory. ADR-021 through ADR-025 written. |
| 14 | 2026-07-21 | Phase 2 implementation, Steps 1–10 + Step 11 started. Redis infra, RoutingDirectory/RedisDirectory, GetOrHydrate, ConnectClaims auth, ResolveGame/hydrateGameSession, heartbeat ticker, Dockerfile + nginx + docker-compose cluster profile, WSHandler rewrite, TD-008 closed. ADR-026, ADR-027 logged. Real multi-instance E2E testing found two critical bugs (JoinGame cross-instance failure, CreateGame missing ownership claim) — diagnosed but not yet fixed at session end. |
| 15 | 2026-07-22 to 2026-07-29 | Full codebase re-analysis before touching anything (all `.go` source, not just docs) per explicit user instruction, rather than trusting the prior session's bug diagnosis. Found the prior session's planned fix for JoinGame (`GetOrHydrate` fallback, matching `HandleConnect`'s Step 8 fix) would have reintroduced split-brain risk on a code path with no ownership-claim precondition — implemented the correct fix instead (pure DB operation, ADR-028), plus the CreateGame ownership-claim fix. Found and fixed two flaky-test bugs unrelated to the main work (JWT tampered-signature test, base64 don't-care-bits). Built `e2e-phase2.sh`, a full scripted E2E harness, iterated on it through several real bugs (a syntax typo, a connection-readiness race fixed via polling instead of blind `sleep`). User's own manual E2E testing surfaced a genuine design gap in abandonment semantics: opponent-never-joined should be void (`ABORTED`), not a phantom win/draw — implemented ADR-029, closing pre-existing TD-P2-005. User's own testing then surfaced a second gap: the abandonment timer doesn't survive instance failover — implemented ADR-030 (persisted disconnect timestamps, deliberately chosen as Postgres over Redis after explicit user request to double-check that choice). User then identified, from first principles, a case ADR-030 alone didn't cover (crash while both players connected, neither disconnect individually observed) — handed to an independently-briefed review session (deliberately not the same session, to avoid the same design blind spot reviewing itself) which confirmed the gap and designed the fix, implemented here as ADR-031. Wrote unit tests and two-Manager-instance integration regression tests for both the new fix and the already-correct case, catching and fixing two of the session's own test-setup bugs in the process (DB/in-memory status desync, Postgres timestamp precision truncation) via the user's actual `go test -race -tags integration` output — not assumed passing. Added Scenario 6 to both `e2e-phase2.sh` and `phase2_step11_e2e_walkthrough.md` for ABORTED, user-confirmed manually. Full Step 12 documentation pass: `ARCHITECTURE.md`, `PHASE_2.md`, `PHASE_3.md` (new open-question note on matchmaking's different abandonment-grace-period shape, explicitly deferred to Phase 3 pre-planning, not resolved here), this file. |
| 16 | 2026-08-08 to 2026-08-09 | **Phase 3 pre-planning, full cycle.** Started this file's own prescribed next step: an independent pre-planning audit. Discovered mid-session that a prior Phase 3 pre-planning attempt existed in memory/context (ADR-032 through ADR-036, an SSE+Redis-pub/sub/Lua-pairing design, `ActiveGameIndex`) but had been fully discarded on 2026-08-06 for hallucinated, unverified content — confirmed independently against the actual repository (`PHASE_3.md`, `CLAUDE.md`, `ARCHITECTURE.md` on disk contained none of it) before proceeding, per explicit user instruction to disregard rather than build on it. Ran the full design pass from scratch: category decision (separate service vs. in-process, server-picks vs. service-assigns — user's own SSE cross-instance-routing objection was the deciding argument, not the initial recommendation, which had favored in-process before that objection was traced through), match-report RPC contract, re-enqueue ownership (user correctly identified the "which component re-enqueues" tradeoff; tracing it found the real hazard was ACK-loss ambiguity, not the race initially suspected, leading to the `matchmaking_request_id` idempotency fix), SSE token/endpoint design, and check-before-enqueue (user's proposed heartbeat-renewed Redis marker, refined from an initial explicit-clear-plus-reconciliation-worker design that would have needed a new background worker, to a lease pattern reusing the existing heartbeat ticker and its established TTL constants). Wrote `PHASE_3_DESIGN_NOTES.md` (16 sections) and `DECISIONS_LOG_PHASE_3.md` (ADR-032 through ADR-038, this file's first-ever Phase 3 entries). Generated a review prompt and handed ADR-032–038 to a separately-briefed independent review session per Constraint 15. That session's reconciliation (`PHASE_3.md`/`ARCHITECTURE.md` rewrites) was itself re-verified rather than trusted: two specific corrected claims (UUID v7, `ConnectClaimsTTL`) confirmed accurate via direct source re-read; a third claim (pub/sub "evaluated during design") challenged on the reviewing session's own first-hand knowledge of the real design history, resolved as a deliberate, disclosed paper-trail choice rather than contamination, and redrafted with honest tense as ADR-040. A further pass over the reconciliation's own output (not assuming it was final just because a review had happened) found and fixed a genuine `CODING_GUIDELINES.md` §7 envelope-conformance bug across three documents, then extended the same fix to `ARCHITECTURE.md`'s older, already-shipped Phase 1/2 endpoint examples for full project-wide consistency, on explicit user confirmation. Traced the still-open connection-grace-semantics question (flagged in this file since Session 15) against actual `HandleConnect`/`HandleDisconnect`/`onAbandonTimeout` code rather than leaving it as an assumption — confirmed one half is already correctly handled by existing code with no new work needed, and the other half is genuinely unaddressed; formalized as TD-P3-004 with an explicit implementation gate, deliberately deferred to its own future session per user's explicit decision, not left as an implicit gap. Nine ADRs total (ADR-032–040) now govern Phase 3's design. Implementation has not started. |

---

## Next Session

**Phase 3 pre-planning is complete and reconciled — this file's prior "run an independent audit first" instruction has been satisfied.** Implementation can begin at Step 1 of `phases/current/PHASE_3.md`'s Implementation Checklist (DB migration: `matchmaking_request_id` column; proto definitions) and Step 2 (nginx config: new `location /matchmaking` block, `proxy_buffering off`, extended `proxy_read_timeout`) — neither touches the still-open connection-lifecycle question. **Step 3 carries an explicit gate**: do not implement `Manager.CreateMatchedGame`'s connection handling until TD-P3-004 (matched player connects and stays connected, assigned opponent never connects at all — no timer currently arms) has its own dedicated session, entangling `HandleConnect`/`HandleDisconnect`, ADR-030/031's failover grace-period persistence, and the existing abandonment timer machinery together rather than being bolted on to one path in isolation. User has explicitly confirmed this will be picked up afterward, deliberately deferred, not forgotten.
