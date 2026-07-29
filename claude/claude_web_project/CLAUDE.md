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

**Phase 3 — Matchmaking: ⬜ Not started.** Before beginning implementation, run an independent pre-planning audit of `PHASE_3.md` against Phase 2's actual final shape — see "Next Session" at the bottom of this file for the exact prompt to use. Phase 2 changed substantially during implementation (co-located sessions, resolve-then-connect, `ABORTED` status, persisted disconnect timestamps) in ways `PHASE_3.md` was only partially updated to account for; do not trust `PHASE_3.md` as settled until that audit runs, the same way `PHASE_2.md`'s own original design was audited before Phase 2 implementation began (ADR-021).

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

## Non-Negotiable Constraints

Unchanged from prior sessions (1–13), still locked. See prior CLAUDE.md revisions for the full list. Notably still governing this session's work:

12. **A context's cancellation timing chosen for one operation must be re-verified for every other operation later wired through the same context — never assumed safe by inheritance.** (ADR-019) Invoked again this session for `HandleDisconnect`'s disconnect-timestamp persist write (ADR-030) — correctly detached, same pattern as the existing clock-persist write.

13. **Any fix applied to one code path handling a shared cross-cutting concern must be checked against every other code path with the same structural dependency, not assumed to be the only place it's needed.** (Formalized this session from prior session's observation about `HandleConnect`'s `GetOrHydrate` fix vs. `JoinGame`'s missed identical need — see Critical bugs #1 above, which is exactly this pattern recurring.)

14. **Single-`Manager`-instance tests cannot validate cross-instance behavior, full stop — this is now a standing testing requirement, not a one-off lesson.** Any test asserting behavior that's supposed to work "across instances" (routing, ownership handoff, registry-miss recovery, abandonment-timer resumption) must construct **two separate `Manager` instances** sharing the same `testPool`/`testRedisClient` (`newTestManagerWithDirectory`, `internal/game/resolve_test.go`). This is not optional polish — every one of Phase 2's three critical-bug rounds this session was invisible to every existing single-`Manager` test suite and only surfaced via the real Docker cluster.

15. **A design's own author is a bad auditor of that same design under time pressure — for genuinely consequential cross-cutting fixes (not routine bugs), get an independently-briefed second pass before implementing, not after.** This session's ADR-030 (persisted disconnect timestamps) shipped with a real, non-trivial gap (the crash-while-both-connected case) that survived one full round of "explain the design back to the user" and direct source-reading. It was caught by deliberately handing the problem to a *separate, neutrally-briefed* review — explicitly instructed not to defer to the first session's own design — rather than by the same session re-checking its own work. When a fix is this consequential (touches failure-mode correctness, not a routine CRUD bug), prefer this pattern proactively rather than discovering the need for it after the fact.

---

## Known Sharp Edges

(All prior sharp edges from earlier sessions remain valid — see prior CLAUDE.md revisions for the full list: migrate CLI URL scheme, `notnil/chess` panics, `ReadLoop` context lifetime, `HandleDisconnect`'s detached clock-persist context, `CloseConnections` ordering requirement, goleak `IgnoreCurrent()` pattern, `gopls` unreliability on filesystem-MCP writes and integration-tagged files, Redis client test isolation via DB 1, `Connection.Close()`'s nil-`wsConn` panic risk, `ConnectClaimsTTL` timing for manual testing, cross-instance bugs needing two-`Manager` tests, etc.)

**New this session:**

- **`session.Transition(newStatus)` only mutates in-memory state — it never touches the database.** Any test that needs a DB row to actually reflect a status (because the code under test does a CAS keyed on that DB status, e.g. `onAbandonTimeout`'s `UpdateGameStatus(fromStatus=ACTIVE, ...)`) must *also* call `gameStore.UpdateGameStatus(...)` directly to sync the DB, or the CAS will correctly (and confusingly) reject the write. Caught via a real, misleading-looking test failure this session.
- **Postgres `TIMESTAMPTZ` only stores microsecond precision; Go's `time.Time` carries nanosecond precision plus an in-process-only monotonic clock reading.** Any test comparing a `time.Now()`-derived value against one that round-tripped through the database must `.Truncate(time.Microsecond)` the value *before* writing it, or an exact `.Equal()` comparison will fail on the lost sub-microsecond digits even though the values are, for any practical purpose, the same instant.
- **`Manager.abandonTimers` is pure per-process memory, full stop.** It does not survive the owning process dying under any circumstance — not a graceful shutdown that doesn't finish in time, not a hard kill, not a crash. Whether a disconnect gets individually recorded to Postgres (ADR-030) before that happens depends entirely on whether `HandleDisconnect` got a chance to run — which it does not, for either player, if the *instance itself* dies while both are still connected. This was the exact gap ADR-031 closed; don't re-derive "does the timestamp survive a crash" reasoning from ADR-030 alone without also reading ADR-031.
- **`Filesystem` MCP tools (`Filesystem:*`) reach the user's actual WSL filesystem; the bare `create_file`/`str_replace` tools reach Claude's own sandbox and produce files the user cannot see or download.** Confirmed the hard way this session — two migration files were created via the wrong tool early in the session and had to be redone via `Filesystem:write_file` once the user reported they couldn't find them. Always use the `Filesystem:`-prefixed tools for any edit intended to land in the real repository.
- **`edit_file`'s `oldText` must match the file's *actual current* content exactly, including subtle box-drawing/whitespace in ASCII diagrams.** Several edits to `ARCHITECTURE.md` this session failed on a plausible-looking `oldText` reconstructed from memory of an earlier read; re-reading the file immediately before editing (not relying on an earlier turn's cached view) resolved it every time. Prefer smaller, more surgical anchors (a unique sentence, not a whole ASCII diagram) when precision is uncertain.
- **`go test -run` does not OR multiple `-run` flags together** — passing it twice silently uses only the last occurrence (standard Go flag package behavior for repeated flags). Use one `-run` with a regex alternation (`-run 'TestA|TestB'`) instead.
- **`e2e-phase2.sh`'s `ws_disconnect` (`kill $pid`, SIGTERM to the backgrounding subshell) does not produce the same server-side disconnect-detection latency as an interactive `wscat` Ctrl+C.** Confirmed via Scenario 6: manual testing showed correct `ABORTED` behavior; the scripted version showed the game still `WAITING_FOR_PLAYER` at the same wall-clock offset. Likely explanation (not fully diagnosed, not pursued further per explicit user decision): the killed subshell probably doesn't get a clean WebSocket close frame out before dying, so the server has to detect a dead TCP connection rather than an explicit close, which takes measurably longer. Tooling-only, not a product defect — noted so a future session doesn't misdiagnose it as a regression.

---

## WebSocket Message Protocol (Phase 2)

Unchanged from the prior session's summary — see that section's content if a future session needs the full detail (route `/connect/{instanceLabel}?token=<connectToken>`, `ConnectClaims` fields, `CONNECT_TOKEN_EXPIRED` error code, two-step client flow). No protocol-level changes this session — ADR-028/029/030/031 all change server-side behavior and DB schema, not the wire format, with one addition: `GAME_OVER`'s `reason` field can now carry the wire-only string `"ABORTED"` (not routed through `store.OutcomeReason`, which has no such DB-level member — `outcome`/`outcome_reason` both stay `NULL` in the database for this case) alongside `outcome: ""`.

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

FLAGGED FOR PHASE 3 PRE-PLANNING (not technical debt, a design question — see PHASE_3.md's "Open question" note added this session): under matchmaking, both players are pre-assigned at match time, so the ADR-029 "opponent never joined the game at all" trigger doesn't have a direct analog — the analogous case is "one matched player connects, the other never does," which may warrant its own (likely shorter) grace period, and specifically should NOT start any timer at all if the connected player also leaves before the opponent ever connects. Do not silently extend ADR-029/030/031's mechanism to cover this without re-deriving it against matchmaking's actual constraints.
```

---

## Session Log

| Session | Date | What Was Done |
|---------|------|----------------|
| 1–12 | 2025-01-XX to 2026-07-10 | Phase 1 complete (see prior CLAUDE.md revisions for full detail) |
| 13 | 2026-07-15 | Phase 2 architecture design: audited original Redis pub/sub plan, found critical bugs, rejected ID-prefix and consistent-hash-ring alternatives, arrived at co-located sessions + Redis routing directory. ADR-021 through ADR-025 written. |
| 14 | 2026-07-21 | Phase 2 implementation, Steps 1–10 + Step 11 started. Redis infra, RoutingDirectory/RedisDirectory, GetOrHydrate, ConnectClaims auth, ResolveGame/hydrateGameSession, heartbeat ticker, Dockerfile + nginx + docker-compose cluster profile, WSHandler rewrite, TD-008 closed. ADR-026, ADR-027 logged. Real multi-instance E2E testing found two critical bugs (JoinGame cross-instance failure, CreateGame missing ownership claim) — diagnosed but not yet fixed at session end. |
| 15 | 2026-07-22 to 2026-07-29 | **This session — the long one.** Full codebase re-analysis before touching anything (all `.go` source, not just docs) per explicit user instruction, rather than trusting the prior session's bug diagnosis. Found the prior session's planned fix for JoinGame (`GetOrHydrate` fallback, matching `HandleConnect`'s Step 8 fix) would have reintroduced split-brain risk on a code path with no ownership-claim precondition — implemented the correct fix instead (pure DB operation, ADR-028), plus the CreateGame ownership-claim fix. Found and fixed two flaky-test bugs unrelated to the main work (JWT tampered-signature test, base64 don't-care-bits). Built `e2e-phase2.sh`, a full scripted E2E harness, iterated on it through several real bugs (a syntax typo, a connection-readiness race fixed via polling instead of blind `sleep`). User's own manual E2E testing surfaced a genuine design gap in abandonment semantics: opponent-never-joined should be void (`ABORTED`), not a phantom win/draw — implemented ADR-029, closing pre-existing TD-P2-005. User's own testing then surfaced a second gap: the abandonment timer doesn't survive instance failover — implemented ADR-030 (persisted disconnect timestamps, deliberately chosen as Postgres over Redis after explicit user request to double-check that choice). User then identified, from first principles, a case ADR-030 alone didn't cover (crash while both players connected, neither disconnect individually observed) — handed to an independently-briefed review session (deliberately not the same session, to avoid the same design blind spot reviewing itself) which confirmed the gap and designed the fix, implemented here as ADR-031. Wrote unit tests (session-machine table extensions, pure-function tests for the new timer math) and two-Manager-instance integration regression tests for both the new fix and the already-correct case, catching and fixing two of my own test-setup bugs in the process (DB/in-memory status desync, Postgres timestamp precision truncation) via the user's actual `go test -race -tags integration` output — not assumed passing. Added Scenario 6 to both `e2e-phase2.sh` and `phase2_step11_e2e_walkthrough.md` for ABORTED, user-confirmed manually (script version has a known, deliberately-not-pursued tooling discrepancy, see Known Sharp Edges). Full Step 12 documentation pass: `ARCHITECTURE.md` (System Overview, endpoints, WebSocket lifecycle, Game State Machine, Database Schema, Dependency Graph all brought current for Phase 2 as actually built), `PHASE_2.md` (checklist, Acceptance Criteria, Technical Debt table all updated against actual results), `PHASE_3.md` (new open-question note on matchmaking's different abandonment-grace-period shape, explicitly deferred to Phase 3 pre-planning, not resolved here), this file. |

---

## Next Session

**Do not begin Phase 3 implementation directly from `PHASE_3.md` as currently written.** Phase 2 changed substantially enough during implementation (co-located sessions were always the design, but the resolve-then-connect mechanics, the `ABORTED` status, and persisted disconnect-timestamp semantics are all new since `PHASE_3.md` was last substantively edited) that an independent pre-planning audit — mirroring the ADR-021 audit that caught Phase 2's original design before implementation began — should run first, in a fresh session, evaluating `PHASE_3.md` against the actual current codebase rather than trusting the document. See the pre-planning prompt drafted for this purpose (ask the user if it isn't already saved in `claude/prompts/PROMPTS.md`).
