# Phase 2 — Horizontal Scaling

**Status: ✅ Complete — implemented, all bugs found via E2E testing fixed, Acceptance Criteria confirmed manually and via `e2e-phase2.sh` (scenarios 0–6)**
**Prerequisite: Phase 1 all acceptance criteria met**

This design supersedes the original Redis-pub/sub cross-instance plan (see
`PHASE_2-deferred.md`) and the ID-prefix and consistent-hash-ring sticky-session
variants considered and rejected during design. Full reasoning trail:
`DECISIONS_LOG_PHASE_2.md`, ADR-021 through ADR-031 — ADR-021–025 predate
implementation (design phase); ADR-026–031 were written during implementation
and E2E testing, correcting real bugs this design's own audit did not catch
(see Implementation Checklist and Acceptance Criteria below for what each one
fixed).

---

## Objective

Run multiple instances of the chess server behind a load balancer. Players
connected via different resolve calls must always land on the same instance as
their opponent, with no degradation in behavior, and no silent breakage when an
instance crashes mid-game.

**The single most important learning outcome of Phase 2:**
A 1:1, turn-based, stateful session has exactly one correct owner at a time.
Cross-instance work for this class of problem is *routing* — getting a connection
to the right owner — not *state synchronization* across independent copies.
Confusing the two is what produces split-brain bugs; keeping them separate is what
avoids them.

---

## The Problem This Phase Solves

In Phase 1, all state lives in one process. Player A and Player B are both
connected to Server Instance 1. Everything works.

Run two instances and the naive version breaks in two different ways depending on
how you try to fix it:

- **Do nothing:** Player A and Player B may land on different instances with no
  way for one to reach the other. The game breaks silently.
- **Fix it with an ID scheme or a hash ring that decides routing from the gameID
  alone:** this works until an instance crashes, fails over, and then *recovers*.
  At that point the ID/ring still points new connection attempts back at the
  recovered instance, which has no memory of the failover and will happily
  reconstruct its own independent, diverging copy of the game — the same "game is
  silently broken" failure, just reintroduced by the fix instead of by the
  original problem. This was found and rejected during design (see ADR-021).

The correct fix routes based on **who currently, actually owns the game** — a
fact that only changes on deliberate reassignment (creation, genuine failover),
never as a side effect of which instances happen to be alive at a given moment.

---

## Architecture Summary

**Co-location, not state sync.** Both players of a game are always served by the
same instance. There is only ever one live `GameSession` for a game, on one
process, at any moment. Cross-instance work is limited to answering "which
instance is that" — never synchronizing board state, clocks, or connections
across processes.

**Two Redis keys per game, decoupled on purpose:**
- `game:{gameID} → instanceID`, `EX 30`, renewed every 10s — the **ownership**
  record. Stable by design; not the failure-detection signal.
- `instance_alive:{instanceID}`, `EX 10`, renewed every 3s — the **liveness**
  record, one per instance regardless of how many games it hosts. This is the
  actual failure-detection signal, checked before a resolve call acts on an
  ownership record it can't otherwise verify. See ADR-023 for why these are two
  keys, not one, and why the liveness TTL is deliberately not tighter than this
  (false-positive risk — see "Known Limitations" below).

**Resolve-then-connect, not connect-then-relay.** The client never dials an
instance blind. It resolves the correct instance first via a plain REST call,
gets back a short-lived, masked routing credential, and only then opens the
WebSocket — to the right instance on the first attempt in the overwhelming
majority of cases. See "Connection Flow" below.

**No Postgres schema change.** Ownership lives entirely in Redis. `games.id`
remains a plain, unencoded `UUID` — no prefix, no suffix, no embedded routing
information of any kind.

**No `RedisEventBus`.** `LocalEventBus` (Phase 1) remains correct indefinitely
under co-location — there is never a second process holding state for the same
game that needs to be notified. ADR-010's Phase 2 half is superseded by ADR-021.

---

## Scope

### In Scope

- `internal/game/directory.go`: `RoutingDirectory` interface + `RedisDirectory`
  implementation (mirrors how `EventBus`/`LocalEventBus` already live together)
- New short-lived `ConnectClaims` JWT type in `internal/auth`, alongside the
  existing long-lived `PlayerClaims` (unchanged)
- New endpoint: `GET /games/:id/resolve`
- Per-instance heartbeat ticker (ownership renewal + liveness renewal, one loop,
  two Redis writes) started in `main.go`, stopped in the shutdown sequence
- `GameRegistry.GetOrHydrate`: atomic single-flight hydrate-on-miss, closing the
  double-registration race a naive registry-miss check would reopen (same class of
  bug ADR-017 already fixed once for first-connect)
- nginx: plain round-robin for REST; a static label→upstream `map` for the WS Edge
  Proxy (mechanical dereference only — no Redis/DB access at the proxy layer)
- One-shot pre-deploy migration service in `docker-compose.yml`, closing TD-008
  (see ADR-025)
- Graceful-shutdown release of an instance's own Redis directory entries (don't
  make a deliberate scale-down wait out a TTL)
- `GameStore.UpdateGameStatus`'s missing status predicate fixed (pre-existing bug,
  independent of this design, found during the Phase 2 code audit)

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| Kubernetes, etcd, Consul | Deferred to Phase 8 — see that file for reasoning. Redis is the deliberately simpler tool for learning the lease/ownership concept itself; the stronger-consistency versions are a good *second* pass, not the first. |
| Fencing tokens / ownership epochs | The textbook-complete fix for the residual false-positive liveness risk (see "Known Limitations"). Not built now — narrow, TTL-bounded window, real complexity, matches this project's standing discipline against solving races that aren't demonstrated to matter (ADR-014, ADR-016). Candidate for Phase 8, where k8s `Lease` gives this almost for free. |
| Active liveness probing (instance-to-instance HTTP health checks) | Considered and rejected in favor of the two-key Redis design — see ADR-023. |
| Eager `RestoreActiveGames`-at-startup | Deliberately dropped for Phase 2 — see ADR-024. Lazy hydrate-on-miss already has to exist; a second, overlapping "restore everything at boot" path adds nothing. |
| Deterministic/eager game rebalancing on instance death | Statistical spread via round-robin is judged sufficient; a reverse index (`instance:{id} → set of gameIDs`) would be needed to build this later if it's ever shown to matter. Not built speculatively. |

---

## Connection Flow

Every WS connect or reconnect — first connect after creation and every later
reconnect — uses the **identical path**, deliberately, with no special-casing:

1. Client calls `GET /games/:id/resolve` with its existing `playerToken`
   (long-lived, from creation/join, unchanged). Round-robins to any instance Z.
2. Z verifies `playerToken` (`auth.VerifyPlayerToken`, unchanged).
3. Z reads `game:{id}` from Redis.
   - **Hit, and `instance_alive:{owner}` present** → mint
     `ConnectClaims{gameID, userID, color, instanceLabel: owner, exp: +10s}`,
     return it plus the masked WS URL `wss://mygame.com/connect/{owner}`.
   - **Miss, expired, or `instance_alive:{owner}` absent (owner confirmed dead
     even if its 30s ownership record hasn't technically lapsed yet)** → Z
     atomically claims ownership (idempotent whether or not it already held it),
     hydrates via `GetGame` + `GetMovesForGame` + `chess.GameFromMoves` +
     `NewGameSessionFromDB` (the same machinery Phase 1's `RestoreActiveGames`
     already has), registers locally through `GetOrHydrate`'s single-flight lock,
     mints `ConnectClaims{instanceLabel: Z}`.
4. Client dials the masked URL. The **Edge Proxy** (nginx) mechanically maps
   `{instanceLabel}` to an internal upstream via Docker Compose service DNS — no
   Redis, no DB, no decision-making at this layer.
5. Target instance verifies `ConnectClaims`, then runs **unmodified Phase 1
   code**: `registry.Get` (should hit), `RegisterConnection` (ADR-017,
   untouched), `GAME_STATE`/`OPPONENT_RECONNECTED`, clock start/resume. If
   `registry.Get` still misses (narrow race inside the 10s window, or a
   fast-restart gap — see ADR-024), the same `GetOrHydrate` path runs as a
   safety net, regardless of cause.

`POST /games/:id/join` and `GET /games/:id` are pure DB operations that never
touch a live `GameSession` — round-robin, no affinity needed, no resolve step
involved. **This was violated in the initial implementation** (`JoinGame`
called `registry.Get` and touched the live session) and is the root cause of
CRITICAL-1 below — fixed by ADR-028, which brought the code in line with this
already-correct design statement rather than changing the design to match the
broken code.

---

## Server-Dies-Mid-Game — Three Scenarios (all must be covered by tests)

1. **Failover, both players eventually reconnect.** First reconnect may hit a
   stale ownership record but a missing liveness key, triggering immediate
   failover rather than waiting out the ownership TTL. Second player resolves
   fresh, lands directly on the correct instance, hits the ordinary Phase 1
   reconnect branch unmodified.
2. **Failover, origin instance recovers afterward.** Must be a non-event.
   Ownership is Redis-truth; the recovered instance has no path to receiving
   traffic for this specific game unless it legitimately re-wins the claim. This
   is the direct test for the split-brain bug the original ID-prefix and
   consistent-hash designs both had.
3. **Failover for one player, the other never reconnects.** Indistinguishable
   from an ordinary Phase 1 single-player disconnect from the new instance's
   perspective. Existing 60s abandon timer and ADR-015's asymmetric outcome logic
   apply unmodified.

---

## Known Limitations (documented honestly, not silently solved)

**False-positive liveness signal can, in a narrow window, split a genuinely live
game.** If an instance's `instance_alive` key transiently lapses (GC pause, brief
Redis hiccup) while it's actually still serving live connections, and a *different*
player's independent reconnect attempt lands on another instance during that exact
window, that instance will incorrectly conclude the original is dead and hydrate a
second, competing `GameSession`. The 3s/10s heartbeat/TTL numbers are chosen
specifically to make this window narrow (comfortably longer than a GC pause or a
momentary Redis blip), not to make it disappear. The complete fix is a fencing
token / ownership epoch, deliberately deferred — see "Explicitly Out of Scope"
above.

**Redis being down blocks new routing decisions, not live gameplay.** Already-
connected players are unaffected — ongoing gameplay never touches Redis, only
resolve calls do. Materially better degradation than the original Redis-pub/sub
design, where a Redis outage broke live cross-instance delivery directly.

**Static Edge Proxy config.** Adding an instance requires an nginx config change
and reload. No dynamic service discovery at the proxy layer in this phase — that
capability is part of what Phase 8 (Kubernetes + ingress) buys.

---

## Implementation Checklist

### Step 1: Redis Infrastructure
- [x] Add Redis service to `docker-compose.yml`
- [x] Add Redis client dependency
- [x] `INSTANCE_ID` config value per replica
- [x] Verify Redis connection on startup; instance can still serve *already-owned,
      already-hydrated* games if Redis is briefly unavailable — new resolves fail
      cleanly, not a crash

### Step 2: Routing Directory
- [x] `internal/game/directory.go`: `RoutingDirectory` interface
- [x] `RedisDirectory`: `ClaimOwnership`, `GetOwner`, `RenewOwnership`,
      `ReleaseOwnership`, `SetAlive`, `IsAlive`, `RenewAlive`
- [x] Unit tests against a real Redis (integration tag), including a simulated
      concurrent-claim race proving exactly one winner

### Step 3: GetOrHydrate
- [x] `GameRegistry.GetOrHydrate(gameID, hydrateFn)`: single-flight per-key lock,
      closing the double-hydration race
- [x] Regression test: two goroutines racing a miss on the same gameID, verify
      exactly one hydration occurs and both callers get the same session pointer

### Step 4: Auth — ConnectClaims
- [x] `internal/auth`: `ConnectClaims` type, `SignConnectToken`,
      `VerifyConnectToken` (short expiry, same signing key as `PlayerClaims`)
- [x] Unit tests: expiry enforcement, tamper rejection, gameID/color match check
      (the tamper-rejection test cases were themselves flaky for an unrelated
      reason — base64's "don't-care" trailing bits on a last-character
      corruption, fixed mid-Phase-2 by flipping a middle character instead;
      test-only, no production risk)

### Step 5: Resolve Endpoint
- [x] `GET /games/:id/resolve` handler: verify `playerToken`, directory lookup,
      claim-and-hydrate on miss, mint `ConnectClaims`, return masked URL
- [x] Handler tests (httptest)
- [x] Test: `ConnectClaims` expiring in the gap between resolve returning and the
      client actually dialing the WS — `WSHandler` must reject cleanly (not
      panic, not hang), and the expected client behavior (re-call resolve, not
      retry the stale masked URL) should be documented in the WS error response
      itself
- [x] Test: resolving/reconnecting to a game already in a terminal status
      (`COMPLETED`/`ABANDONED`/`ABORTED`) through the hydrate-on-miss path —
      confirms correct final `GAME_STATE` rather than erroring
- [x] **Post-hoc fix (ADR-028):** `CreateGame` did not claim Redis ownership at
      creation time, causing indefinite "lost ownership renewal" heartbeat log
      spam for any created-but-never-resolved game. Fixed: best-effort
      `ClaimOwnership` call immediately after local registration.

### Step 6: Heartbeat Ticker
- [x] Per-instance ticker in `Manager`/`main.go`: batched ownership renewal
      (`registry.AllActive()`) + liveness renewal, every 3s
- [x] Graceful shutdown: release owned entries proactively before exit
- [x] Goroutine-leak test for the ticker itself (this codebase's standing
      discipline — see CLAUDE.md Known Sharp Edges on `goleak.IgnoreCurrent()`)

### Step 7: Edge Proxy / nginx
- [x] `nginx.conf`: round-robin upstream for REST; static `map` + named upstream
      per instance label for `/connect/{instanceLabel}`
- [x] WebSocket upgrade headers (`Upgrade`, `Connection`), long timeouts
- [x] Health checks on `/health`

### Step 8: WSHandler Changes
- [x] Accept `ConnectClaims` instead of `PlayerClaims` on the WS upgrade path
- [x] `registry.Get` → `GetOrHydrate` fallback on miss, regardless of cause
- [x] **Post-hoc fix (ADR-028):** `JoinGame` was missing the equivalent
      cross-instance treatment `HandleConnect` got here — except the correct
      fix was the opposite of what this step's pattern suggests. `JoinGame`
      should never have touched `GameRegistry`/`GameSession` at all (see the
      Connection Flow section above); giving it a `GetOrHydrate` fallback like
      `HandleConnect`'s would have reintroduced split-brain risk on a code path
      with no ownership-claim precondition. Fixed by removing the registry
      touch entirely — pure DB operation, matching this document's own
      original design statement.

### Step 9: TD-008 Resolution
- [x] One-shot `migrate` service in `docker-compose.yml`; server replicas
      `depends_on: condition: service_completed_successfully`

### Step 10: Bug Fix Carried In From Audit
- [x] `GameStore.UpdateGameStatus`: add status predicate to the `WHERE` clause

### Step 11: Multi-Instance Testing (all three scenarios from above, explicitly)
- [x] Two players resolve to different instances via round-robin creation/join,
      confirm both land on the same instance for gameplay
- [x] Kill an instance mid-game; reconnecting player fails over correctly
      (Scenario 1)
- [x] Kill an instance, let it restart, confirm it does **not** re-acquire a game
      that already failed over (Scenario 2 — the split-brain regression test)
- [x] Kill an instance where only one player ever reconnects; confirm normal
      Phase 1 abandonment fires (Scenario 3) — **required two additional fixes
      beyond original scope to actually pass, ADR-029/030/031, see below**
- [x] Kill Redis mid-game; confirm no crash, confirm already-connected players are
      unaffected, confirm new resolves fail cleanly
- [x] Reconnect to an already-completed game via the resolve → hydrate path;
      confirm correct terminal `GAME_STATE` delivery, no error
- [x] **Added beyond original scope — Scenario 6:** opponent never joins at all,
      creator disconnects → confirm `ABORTED`, not stuck in
      `WAITING_FOR_PLAYER` forever and not a phantom win/draw (ADR-029)
- [x] **Added beyond original scope — regression check:** `e2e-phase2.sh
      regress N` — N rapid create+join cycles across both instances,
      confirming CRITICAL-1/2 don't recur under real round-robin (a single
      manual trial can pass by luck even with the bug present, since
      round-robin only lands cross-instance ~50% of the time)

### Step 12: Documentation
- [x] `ARCHITECTURE.md`: System Overview, `internal/api` endpoints, WebSocket
      Connection Lifecycle, Game State Machine (`ABORTED` added), Database
      Schema (disconnect timestamp columns added), Dependency Graph
- [x] `DECISIONS_LOG_PHASE_2.md`: ADR-021 through ADR-031
- [x] Update `CLAUDE.md`
- [x] `phase2_step11_e2e_walkthrough.md` and `e2e-phase2.sh`: Scenario 6 added

### Critical Bugs Found During Step 11 and Fixed (not in original scope)

Three separate rounds of real multi-instance E2E testing surfaced bugs this
design's own upfront audit (ADR-021's design-phase review) did not catch —
none of them were reachable by single-`Manager` integration tests, since a
single `Manager` instance cannot reproduce what nginx's round-robin does
across genuinely separate processes:

- **CRITICAL-1/2 (ADR-028):** `JoinGame` touched `GameRegistry` and failed on
  any cross-instance registry miss (~50% of joins under real round-robin,
  and — found on closer reading — could leave a game permanently unjoinable
  even on the failure path, since the DB write committed before the failure).
  `CreateGame` never claimed Redis ownership, causing indefinite heartbeat log
  spam. Both fixed; `JoinGame` is now a pure DB operation.
- **ABORTED status gap (ADR-029):** a game whose opponent never joined at all
  had no correct terminal outcome — `onAbandonTimeout` tried to run
  `ACTIVE`-game logic (award a phantom win or a meaningless draw) against a
  game that never started, and the state machine had no `WAITING→ABANDONED`
  edge anyway (TD-P2-005). Fixed with a new `ABORTED` status: void, no
  outcome, no reason.
- **Abandonment-timer failover gap (ADR-030/ADR-031):** the 60s abandonment
  timer is pure in-process memory and does not survive the owning instance
  dying. First fix (ADR-030) persisted the individually-observed disconnect
  moment to Postgres so a survivor could resume it — correct for "a player
  disconnected while the instance was still alive," incomplete for "the
  instance itself died while both players were still connected," which never
  gets a persisted timestamp at all since nothing witnesses it. Second fix
  (ADR-031, found by an independently-briefed review session, not this one)
  closed that gap: assume disconnected as of the hydration moment when no
  timestamp exists for a non-terminal game, rather than assuming fine.

---

## Acceptance Criteria

| # | Criterion | Result |
|---|-----------|--------|
| 1 | Two players, connecting independently, always end up co-located on the same instance | ✅ Confirmed (Scenario 0, and `regress N`) |
| 2 | All Phase 1 acceptance criteria still pass under the multi-instance setup | ✅ Confirmed |
| 3 | Scenario 1 (failover, both reconnect): game resumes correctly on the new owner | ✅ Confirmed |
| 4 | Scenario 2 (failover, origin recovers): recovered instance never re-hydrates a migrated game — no split game under any timing | ✅ Confirmed |
| 5 | Scenario 3 (failover, one player never returns): normal abandonment semantics apply, unmodified | ✅ Confirmed — required ADR-029/030/031, not "unmodified" as originally worded; see Critical Bugs above |
| 6 | Redis going down does not crash any instance; live gameplay is unaffected; new resolves fail cleanly | ✅ Confirmed (Scenario 4) |
| 7 | Edge Proxy correctly proxies the WebSocket upgrade for a masked-URL connection | ✅ Confirmed |
| 8 | All tests pass: `go test -race ./...`, including the `GetOrHydrate` single-flight regression test | ✅ Confirmed, including `-tags integration -race` |
| 9 | `TD-008` closed: concurrent replica startup never double-runs migrations | ✅ Confirmed |
| 10 | `GameStore.UpdateGameStatus`'s status predicate fix verified with a regression test | ✅ Confirmed |
| 11 | *(Added, ADR-029)* Opponent never joins, creator disconnects: game reaches `ABORTED` — no outcome, no outcome_reason, not stuck in `WAITING_FOR_PLAYER` forever | ✅ Confirmed manually (Scenario 6); scripted `scenario6` shows a known tooling-only discrepancy (`ws_disconnect`'s `kill` vs. an interactive Ctrl+C's clean close frame) — not a product issue, not pursued further |

---

## Technical Debt This Phase Introduced

| ID | Description | Status | Must Fix By |
|----|-------------|--------|-------------|
| TD-P2-001 | No fencing token — narrow, TTL-bounded false-positive liveness window can theoretically split a live game. **Addendum (ADR-031):** under this same window, a phantom competing session can now also spuriously abandon-lose a genuinely-connected player, not just cause duplicate processing — same bounded window, an additional possible symptom. | Open | Phase 8 (k8s `Lease` gives this near-free) |
| TD-P2-002 | Static Edge Proxy config — scaling requires a config edit + reload | Open | Phase 8 (dynamic service discovery) |
| TD-P2-003 | No active liveness probing — detection is purely TTL-based | Open | Revisit only if TD-P2-001's window is shown to matter in practice |
| TD-P2-004 | A session hydrated via resolve for an already-terminal game stays registered in the local `GameRegistry` indefinitely — nothing currently evicts it | Open | Not scheduled; revisit if shown to matter |
| TD-P2-005 | `onAbandonTimeout`'s both-disconnected branch could never succeed for a `WAITING_FOR_PLAYER` game (no `WAITING→ABANDONED` edge) | **Closed (ADR-029)** | — |
| TD-P2-006 | A game where nobody ever calls `/resolve` again after an instance dies is not covered by ADR-030/031's fix, and cannot be by construction — nothing runs if nobody triggers a hydrate | Open | Not scheduled; candidate design is a periodic sweep folded into the existing heartbeat tick, not built speculatively |
