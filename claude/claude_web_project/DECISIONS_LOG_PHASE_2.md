# Architecture Decision Records — Phase 2

Continuation of `DECISIONS_LOG_PHASE_1.md`'s numbering (ADR-021 onward) in a
separate file, split by phase for readability. Numbering is global and
sequential across both files — there is exactly one ADR-021 in this project, and
it lives here, not in the Phase 1 file. Same format, same append-only discipline:
entries are never edited after acceptance; reversals get a new ADR that
supersedes the old one by reference.

---

## ADR-021: Phase 2 Cross-Instance Architecture — Co-Located Sessions with a
Redis-Backed Ownership/Liveness Routing Directory

**Date:** 2026-07-15
**Status:** ACCEPTED

**Context:**

`ROADMAP.md`'s original Phase 2 section specified Redis pub/sub as the
cross-instance event bus, with two server instances each independently
maintaining a `GameSession` for a shared game and forwarding events to each
other via Redis. Before implementation began, this design was audited against
the actual codebase (`PHASE_2_FLOW_WALKTHROUGH.md`) and found to have several
critical, not incidental, bugs:

- `UpdateGameStatus` had no status predicate in its `WHERE` clause, contradicting
  the walkthrough's assumptions about safe concurrent terminal-state writes.
- The proposed cross-instance activation mechanism would deterministically
  deadlock every cross-instance game — the non-activating instance's local
  session never transitioned to `ACTIVE`.
- All four terminal-state handlers in the walkthrough unconditionally mutated
  local state before checking DB write results.
- `JoinGame` broke entirely under deferred session creation.
- `finalizeGame` was never called on the non-authoring instance, leaking
  registry memory.
- `IsPlayerConnected` relied on local connection pointers that are structurally
  nil for a player connected to a different instance, producing incorrect
  abandonment decisions and silently dropped notifications.

The root cause common to all of these: the design required two independent
processes, each holding an independently-constructed `GameSession` for the same
game, to converge on one truth. That is a distributed-consensus problem hiding
inside what is structurally a single-writer session — a chess game is a 1:1,
turn-based, exactly-two-participant session with no natural reason for its
authoritative state to exist in more than one place at a time.

Three alternative designs were subsequently proposed and rejected in sequence
before arriving at the accepted design (see also `DECISIONS_LOG_PHASE_1.md`
ADR-009 through ADR-017, whose atomicity patterns this decision reuses rather
than replaces):

**Option A: Redis pub/sub cross-instance event bus (original design).**
Rejected — see bug list above. The fundamental flaw (two independent authoritative
copies of one game) cannot be patched incrementally; it requires a different
architecture, not a bugfix pass.

**Option B: Sticky sessions via server-ID-prefixed game IDs
(`s1_<uuidv7>`), load balancer parses the prefix to route.**
Proposed, reviewed against actual source, rejected. `games.id` and
`moves.game_id` are native Postgres `UUID` columns (confirmed by reading
`migrations/002_create_games.up.sql` and `003_create_moves.up.sql`) — a
prefixed string is not a valid UUID literal and fails on every insert as
literally specified; would require either a schema migration to `TEXT` or a
routing-ID/DB-ID split not present in the original proposal. Independently of
that, hydrate-on-miss (required for failover under this design) had no
single-flight protection, reopening the same double-registration race class
`DECISIONS_LOG_PHASE_1.md` ADR-017 already closed once for first-connect. Most
importantly: no mechanism prevented a recovered origin instance from
independently re-hydrating a game that had already migrated to a survivor during
its downtime — reproducing the exact "game silently breaks" failure Phase 2
exists to prevent, just via a different mechanism than Option A.

**Option C: Sticky sessions via consistent-hash ring at the load balancer**
(nginx `hash $gameID consistent`). Proposed as a refinement of Option B (drops
the ID-encoding problem), reviewed, rejected. A consistent-hash ring recomputes
key→node assignment on *any* cluster membership change — not only failures, also
plain scale-up and scale-down. Once a game fails over from S1 to S2, the ring
keeps re-deriving "S1" as the answer for every subsequent connection attempt for
that game's remaining lifetime, once S1 is healthy again — an unbounded,
permanent misrouting tax rather than a rare edge case. This was the key insight
that reframed the problem: routing needs an explicit, deliberately-written
mapping that only changes on deliberate reassignment, never as a side effect of
which nodes happen to be alive at a given moment.

**Decision:** Co-located sessions (both players of a game always served by the
same instance — carried forward from Options B/C, the one part of the sticky-
session instinct that was correct throughout), routed via an explicit Redis-
backed ownership map (`game:{gameID} → instanceID`), not a ring, not an
ID-embedded hint.

**Rationale:**

The correct scaling unit for this problem is the game, not the connection. A
game has exactly one legitimate owner at a time; the job of Phase 2 is to route
each connection attempt to that owner, not to make two owners agree. Co-location
makes the entire Option-A bug class structurally impossible — there is never a
second `GameSession` to diverge from the first. An explicit, write-driven
ownership map (rather than a function recomputed from cluster membership) is the
only one of the three routing mechanisms considered that has no failure mode
triggered by a *healthy* event (a node recovering, a node being added) — its
only failure mode is the map itself becoming briefly stale between a real crash
and TTL expiry, which is bounded and independently addressed (ADR-023).

**Consequences:**
- No Postgres schema change for routing — `games.id` remains a plain, unencoded
  `UUID`. Superseded an earlier intermediate proposal (`host_instance_id`/
  `host_last_seen` columns on `games`) once the design moved from
  connect-then-relay to resolve-then-connect (ADR-022) and ownership tracking
  moved entirely into Redis.
- `LocalEventBus` remains correct indefinitely. `RedisEventBus` is not built.
  This supersedes the Phase 2 half of `DECISIONS_LOG_PHASE_1.md` ADR-010, which
  is not deleted (per this project's append-only discipline) but is no longer
  the plan.
- `ROADMAP.md`'s original framing — "Redis pub/sub is the correct solution at
  this scale," and its explicit rejection of sticky sessions as "a single point
  of failure per user" — is corrected. The single-point-of-failure framing did
  not account for a lease-plus-failover design not actually being a single point
  of failure; it was true of a naive sticky-session design with no failover
  path, which this is not.
- The original Redis-pub/sub design remains on disk at `PHASE_2-deferred.md` for
  history, marked superseded, not deleted.

---

## ADR-022: Resolve-Then-Connect Routing with a Two-Token Split, Not
Connect-Then-Relay

**Date:** 2026-07-15
**Status:** ACCEPTED

**Context:**

Given ADR-021's decision to route via an explicit ownership map, two shapes were
considered for how a client actually reaches the correct instance.

**Option A: Connect-then-relay.** The client always dials the load balancer
directly for the WebSocket upgrade. Whichever instance receives it checks the
directory; if it isn't the owner, it internally relays — proxies or splices the
live WebSocket connection through to the actual owner, transparently to the
client.

- Pros: no new endpoint, no new token type, single request from the client's
  perspective.
- Cons: requires the misrouted instance to maintain a live, full-duration
  bidirectional relay for every connection it doesn't own — on the (N-1)/N
  fraction of connects under plain round-robin, this is the common case, not an
  edge case. The correct stdlib mechanism for this (`httputil.ReverseProxy`'s
  WebSocket handling vs. a manual `net.Conn` splice) was not verified against
  source before this option was written down, and carries real implementation
  risk independent of the routing design's correctness. A bug in the relay path
  would be a new, Phase-2-specific class of connection failure with no Phase 1
  analog to reuse or test against.

**Option B: Resolve-then-connect (CHOSEN).** The client first calls a plain
REST endpoint (`GET /games/:id/resolve`) authenticated with its existing
`playerToken`. Whichever instance answers this either already knows the owner
or determines it (claiming/hydrating if necessary), and returns a short-lived,
masked routing credential naming the correct instance. The client then opens the
WebSocket directly to that instance via a masked URL, dereferenced mechanically
by the Edge Proxy.

- Pros: the WebSocket connection is only ever attempted once, against the
  already-resolved correct target — no server-side relay code exists at all,
  eliminating an entire class of implementation risk. The routing decision and
  the stateful connection are cleanly separated: one is a stateless REST call
  that's trivially retryable, the other is the actual long-lived session.
- Cons: one extra HTTP round-trip before the WebSocket upgrade; a second, new
  token type to manage (mitigated — see Consequences).

**Decision:** Option B — resolve-then-connect, with a two-token split:
long-lived `PlayerClaims` (existing, unchanged) authenticate the resolve call;
a new short-lived `ConnectClaims` (`{gameID, userID, color, instanceLabel},
exp: +10s`) authenticates the WebSocket upgrade itself.

**Rationale:**

Option A's relay requirement was the deciding factor: it is not a rare
fallback path, it is the majority-case path under any LB strategy that doesn't
itself have game-affinity awareness (see ADR-021's rejection of hash-ring
affinity), meaning its correctness would matter on effectively every
connection, not occasionally. Option B removes that risk entirely by construction
rather than by careful implementation of a relay. The two-token split follows
the precedent already established by `DECISIONS_LOG_PHASE_1.md` ADR-008
(stateless JWTs, scoped per purpose) — a second, narrower-scoped, shorter-lived
token type is a natural extension of that pattern, not a deviation from it.
Client-visible topology (which instance owns a game) is never exposed in either
token or endpoint — `instanceLabel` is an opaque identifier meaningful only to
the Edge Proxy's static map, not a real address.

**Consequences:**
- `internal/auth` gains `ConnectClaims`, `SignConnectToken`, `VerifyConnectToken`
  alongside the existing, unmodified `PlayerClaims`/`VerifyPlayerToken`.
- New endpoint `GET /games/:id/resolve`.
- `WSHandler`'s upgrade path now verifies `ConnectClaims`, not `PlayerClaims`
  directly — `PlayerClaims` is checked once, earlier, at resolve time.
- No relay/proxy code exists anywhere in the Go codebase for this purpose. The
  Edge Proxy's label→upstream dereference is pure nginx configuration.
- `POST /games` and `POST /games/:id/join` are unaffected — creation and joining
  are one-shot DB operations that don't need a resolve step; only the
  connection-establishment path does.

---

## ADR-023: Two Separate Redis Keys — Ownership vs. Liveness — Instead of One
Combined Key or Active HTTP Probing

**Date:** 2026-07-15
**Status:** ACCEPTED

**Context:**

An early version of this design used a single Redis key
(`game:{gameID} → instanceID`, one TTL) for both "who owns this game" and "is
that owner still alive." This forces a bad tradeoff: a tight TTL gives fast
failure detection but makes the *ownership* record — which should be stable —
churn on any transient renewal hiccup, risking spurious reassignment; a moderate
TTL keeps ownership stable but slows failure detection, directly reopening the
"S1 recovers but a stale answer is handed out in the meantime" latency the
design is trying to minimize.

**Option A: Active instance-to-instance HTTP health probing.** Before minting a
`ConnectClaims`, the resolving instance makes a direct internal HTTP health call
to the claimed owner.

- Pros: near-eliminates the stale-claim window.
- Cons: introduces a new dependency direction (every instance must accept
  inbound probes from every peer — new internal network/firewall surface in any
  real deployment), and a new failure mode (a slow or timed-out probe adds tail
  latency to `resolve` for a reason unrelated to whether the *routing decision*
  itself needs it). Adds an entirely new call type to reason about, rather than
  reusing the store already central to the design.

**Option B: Two Redis keys, decoupled TTLs (CHOSEN).**
`game:{gameID} → instanceID`, `EX 30`, renewed every 10s (ownership — stable,
not the detection mechanism). `instance_alive:{instanceID}`, `EX 10`, renewed
every 3s, one key per instance regardless of game count (liveness — the actual
detection signal). At resolve time: read the ownership key for a candidate,
then check the liveness key for that candidate; a missing liveness key means
"confirmed dead even though the ownership record hasn't technically expired
yet," triggering immediate reassignment rather than waiting out the 30s
ownership TTL.

- Pros: fast, confident death-detection using the same store already central to
  the design, no new network call type, no new dependency direction. Liveness
  writes are O(instances), not O(games) — cheap regardless of fleet size.
- Cons: a residual false-positive window remains — see below.

**Decision:** Option B.

**Rationale:**

Option B gets the practical benefit Option A was reaching for (fast, confident
detection) without introducing a new call type, a new dependency direction, or
new tail-latency risk on the hot path. It decouples two genuinely different
concerns that a single key was forcing into one tradeoff.

**A residual risk was found and is deliberately not fully closed here:** a
transient false-negative on the liveness key (a GC pause, a momentary Redis
client hiccup) while the owning instance is genuinely still serving live
connections can cause a *different* player's independent reconnect attempt,
landing on another instance during that exact window, to incorrectly conclude
the owner is dead and hydrate a second, competing `GameSession` — splitting a
game that was never actually down. This is not eliminated by Option B, only
made rare, by choosing a liveness TTL (10s) comfortably longer than a typical GC
pause or momentary store hiccup. The complete fix is a fencing token / ownership
epoch (the loser of a race is required to detect it has been superseded and stop
serving) — not built now. This is a deliberate scope decision, not an oversight:
narrow, TTL-bounded window, real implementation complexity, and this project has
already established the discipline of not building distributed-locking
machinery ahead of demonstrated need (`DECISIONS_LOG_PHASE_1.md` ADR-014,
ADR-016). Flagged as TD-P2-001 and as a named candidate for Phase 8, where
Kubernetes' `Lease` object provides `resourceVersion`-based fencing without
hand-built epoch tracking.

**Consequences:**
- `RoutingDirectory` interface gains both ownership and liveness operations
  (`ClaimOwnership`/`GetOwner`/`RenewOwnership`/`ReleaseOwnership` and
  `SetAlive`/`IsAlive`/`RenewAlive`), not just one.
- The per-instance heartbeat ticker does two Redis writes per tick (batched
  ownership renewal across all locally-active games, plus one liveness renewal),
  not one.
- TD-P2-001 (documented, not silently solved) carried forward in `PHASE_2.md`.
- Numbers (10s/30s ownership, 3s/10s liveness) are a deliberate conservative
  choice on the detection-speed-vs-false-positive-risk tradeoff, not an
  arbitrary "moderate" default — tightening either TTL trades faster failover
  for a larger false-positive window and should not be done without
  re-evaluating this tradeoff explicitly.

---

## ADR-024: Drop Eager `RestoreActiveGames`-at-Startup for Phase 2

**Date:** 2026-07-15
**Status:** ACCEPTED

**Context:**

Phase 1's `RestoreActiveGames` unconditionally reloads every `ACTIVE`/
`WAITING_FOR_PLAYER` game from Postgres at server startup — correct there, since
a single-instance deployment has exactly one possible owner for every game, so
restoring everything is always right.

Under Phase 2's co-located, directory-routed design, tracing the fast-restart
case surfaced a specific gap: if an instance crashes and restarts quickly
enough that its Redis ownership entry hasn't yet expired, and no failover has
occurred, Redis still correctly says this instance owns a given game — but the
instance's own in-memory `GameRegistry` is empty (fresh process), so a
`registry.Get` for that game would miss locally despite Redis confirming it's
the rightful owner.

**Option A: Keep eager `RestoreActiveGames`-at-startup, scoped to only the
games this instance is recorded as owning in Redis.** On boot, query Postgres
for active/waiting games, cross-check each against the Redis ownership record,
restore only the ones still legitimately owned.

- Pros: closes the fast-restart gap directly; registry misses become rarer.
- Cons: introduces a second code path that has to stay correct for the same
  guarantee `GetOrHydrate` (ADR-022's connection flow, step 5's fallback) already
  has to provide for ordinary failover. Two paths doing overlapping work for one
  guarantee is exactly the kind of duplication this project has otherwise
  avoided (`CLAUDE.md`'s "single correct code path over topology-specific fast
  paths" principle, stated independently of this decision but directly
  applicable here).

**Option B: Drop eager restore entirely; rely purely on on-demand,
per-connection `GetOrHydrate` (CHOSEN).**

- Pros: one code path, not two. The fast-restart gap resolves for free — a local
  registry miss triggers hydration regardless of *why* it's a miss (never-owned,
  post-failover, or fast-restart-with-empty-registry all look identical to this
  code, and all three are handled correctly by it). Removes an entire category
  of "did startup restore correctly" reasoning from the codebase.
- Cons: games sit dormant, unhydrated in memory, from the moment an instance
  boots until a player's client actually attempts to reconnect. No functional
  cost — reconnection UX is identical either way, since hydration was always
  going to be required as of the first message exchange regardless.

**Decision:** Option B.

**Rationale:**

`GetOrHydrate` already has to exist and already has to be correct for ordinary
mid-game failover (ADR-021, ADR-022). Once that code path exists, restoring
eagerly at startup provides no capability it doesn't already provide on demand —
it only adds a second place the same guarantee could be gotten wrong. This is a
deliberate simplification, not a regression: Phase 1's acceptance criterion
("killing/restarting the server resumes correctly") is preserved exactly, just
satisfied by the connection-time path instead of a boot-time path.

**Consequences:**
- `main.go`'s startup sequence no longer calls `RestoreActiveGames` under the
  Phase 2 wiring. (The function itself is not deleted — it remains correct and
  in use for a genuinely single-instance deployment, e.g. local dev without the
  Phase 2 multi-instance stack.)
- Every local `registry.Get` miss, for any reason, is treated identically:
  routed through `GetOrHydrate`. This is now the single mechanism responsible
  for "does this instance have this game's session in memory," full stop.
- This directly resolves the fast-restart gap traced during design without
  adding new code to resolve it specifically.

---

## ADR-025: TD-008 Resolution — One-Shot Pre-Deploy Migration Service

**Date:** 2026-07-15
**Status:** ACCEPTED

**Context:**

`CLAUDE.md`'s TD-008, open since Phase 1 Step 13, flagged that automatic
migrations on server startup (`runMigrations` in `main.go`) would race under
multiple concurrent instances at boot — advisory-lock contention between
replicas, and DDL privileges held by the same role that runs the application,
a wider blast radius than necessary. This must close before Phase 2 ships
multiple concurrent instances, per CLAUDE.md's own prior note.

**Option A: Keep automatic migrations in `main.go`, rely on
`golang-migrate`'s built-in advisory lock to serialize concurrent attempts.**

- Pros: no new deployment artifact.
- Cons: does not remove the DDL-privilege blast radius concern (the application
  role still needs schema-modification rights indefinitely, not just at deploy
  time). Startup latency for every replica now includes waiting on a lock held
  by whichever replica won the race, coupling application boot time to migration
  duration for every instance, not just one.

**Option B: One-shot pre-deploy migration service, decoupled from application
boot (CHOSEN).** A dedicated `migrate` service in `docker-compose.yml` runs
`migrate up` once and exits `0`. Every server replica's `depends_on` is set to
`condition: service_completed_successfully`, so no replica starts serving
traffic until migrations have completed exactly once, by exactly one process,
before any replica boots.

- Pros: closes both original concerns directly — no concurrent migration
  attempts possible (only one process ever runs `migrate up`), and the
  DDL-privileged credential can be scoped to the migration service alone,
  distinct from the application's runtime credential (a follow-on hardening step,
  not required to close TD-008 itself but enabled by this shape). No replica's
  boot time is coupled to migration duration.
- Cons: one new service definition in `docker-compose.yml`; requires Compose's
  `service_completed_successfully` condition (supported in current Compose
  versions, confirmed available for this project's tooling).

**Decision:** Option B.

**Rationale:**

This was already the "likely fix" CLAUDE.md predicted for TD-008 when it was
first opened during Phase 1; Phase 2 is the point at which it must actually
close, since it's now a correctness blocker, not a deferred concern. Option B
is the standard shape for this problem (migration-as-a-deploy-step, not
migration-as-part-of-boot) and requires no new tooling beyond what
`docker-compose.yml` already supports.

**Consequences:**
- `runMigrations` remains in the codebase (useful for local single-instance dev
  without the full Compose stack) but is not invoked in the Phase 2 multi-
  instance `docker-compose.yml` path — the `migrate` service replaces it there.
- TD-008 is closed by this ADR's implementation, not merely re-deferred.
- Credential-scoping (DDL-privileged role for the migration service only,
  narrower runtime role for the application) is noted as a natural follow-on
  hardening step but is not itself required to close TD-008.

---

## ADR-026: Redis Client Library — redis/go-redis/v9

**Date:** 2026-07-16
**Status:** ACCEPTED

**Context:**

PHASE_2.md Step 1 requires a Redis client dependency for the routing directory
(ownership + liveness keys, ADR-021/ADR-023) — not an EventBus, not a cache in
the general sense. Two mainstream Go Redis clients exist.

**Options Considered:**

**Option A: `redis/go-redis/v9`**
- Pros: Most widely used Go Redis client, native `context.Context` support on
  every command (matches Non-Negotiable Constraint #7 — every I/O function
  takes context first), built-in connection pooling comparable in spirit to
  `pgxpool`, straightforward `Options{Addr: ...}` construction mirroring
  `store.NewPool`'s shape, actively maintained, typed command results reduce
  the same `interface{}`/`any` concerns ADR-011/CODING_GUIDELINES.md §8
  already flag for other dependencies.
- Cons: None specific to this project's needs.

**Option B: `gomodule/redigo`**
- Pros: Simpler, lower-level, smaller API surface.
- Cons: `context.Context` support is bolted on rather than native to the core
  API in the same way as go-redis/v9, and command results are largely
  untyped (`interface{}`), pushing more manual type-assertion boilerplate
  onto every call site — directly working against CODING_GUIDELINES.md §8's
  "no `interface{}`/`any` without justification" rule for no offsetting
  benefit here.

**Decision:** `github.com/redis/go-redis/v9`.

**Rationale:**

Same category of decision as ADR-002 (gorilla/websocket) and ADR-005
(pgx/v5): pick the client whose API shape matches this codebase's existing
discipline (explicit context propagation, typed results, no ad hoc
`interface{}` handling) rather than the more minimal option. This does not
rise to the weight of a full architecture ADR — it is a library selection
with one clearly dominant option — logged here only for the same
completeness convention Phase 1's client/library choices (ADR-002 through
ADR-006) already established.

**Consequences:**
- `github.com/redis/go-redis/v9` added to `go.mod`.
- `internal/game/directory.go`'s `NewRedisClient` (PHASE_2.md Step 1) returns
  `*redis.Client` from this package; Step 2's `RedisDirectory` will build on
  the same client.
- No transaction/pipelining strategy decided yet — deferred to Step 2, where
  `RoutingDirectory`'s actual Redis command usage (`SET ... EX`, `GET`, etc.)
  is designed.

---

## ADR-027: GetOrHydrate's Shared Hydration Uses a Detached, Independently-Bounded Context — Not the Triggering Caller's Own Context

**Date:** 2026-07-17
**Status:** ACCEPTED

**Context:**

`GameRegistry.GetOrHydrate` (PHASE_2.md Step 3) coalesces concurrent
hydrate-on-miss calls for the same `gameID` via `golang.org/x/sync/singleflight`,
so two goroutines racing a miss trigger exactly one hydration and both receive
the same `*GameSession` pointer — closing the double-hydration race the same
way ADR-017 closed first-connect's double-registration race.

`singleflight.Group.Do(key, fn)` runs `fn` exactly once per in-flight key;
every caller racing the same key — the one whose call happened to trigger
`fn`, and every other one that arrived while it was running — blocks on and
receives that single execution's result. `fn` itself, however, only ever runs
with whatever state its actual triggering call closed over. If `hydrateFn`
were invoked with the triggering caller's own request-scoped `context.Context`
(e.g. an HTTP resolve handler's `r.Context()`), then that one caller's context
cancelling — their client disconnecting, their request timing out — would
abort the DB reads for every other caller silently piggybacking on the same
hydration, even though those callers' own underlying requests are still live
and waiting. This is the identical failure shape ADR-019 found for
`HandleDisconnect`'s clock-persist write (a write whose effect is not scoped
to the triggering call's own lifecycle, wired through a context that can be
cancelled by something unrelated to that effect) — recurring here at a new
call site Phase 2 introduces, not yet exercised by any test since Step 5 (the
first real caller of `GetOrHydrate`) is not yet built. Caught during Step 3
design, before implementation, rather than via a failing test.

**Options Considered:**

**Option A: Use the triggering caller's own `ctx` directly for `hydrateFn`.**
- Pros: Simplest; no extra timeout management; a triggering caller's own
  cancellation stops wasted work if *no one* still wants the result.
- Cons: Under Phase 2 specifically, `GetOrHydrate` is the exact mechanism
  that recovers a game after failover — the whole point of the phase. If the
  one request that happens to win the singleflight race is abandoned
  mid-flight (mobile network hiccup, tab closed), every other player or
  instance racing for that same `gameID` sees hydration fail too, for a
  reason that has nothing to do with their own connection. This directly
  undermines the resilience property Phase 2 exists to provide, for a
  possibly-common case (mobile clients), not a rare one.

**Option B: Detach `hydrateFn`'s context from the triggering caller, bounded by its own independent timeout (CHOSEN).**
`context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)`, exactly the
pattern ADR-019 already established, applied at the `hydrateFn` call site
inside the singleflight closure.
- Pros: Hydration's outcome no longer depends on which particular caller
  happened to trigger it — every piggybacking caller gets a
  triggering-caller-independent result. Still bounded (10s), so a genuinely
  stuck DB call cannot hang every racing caller indefinitely — not an
  unbounded escape hatch, same discipline ADR-019 required of itself.
- Cons: A hydration whose sole triggering request was abandoned still runs to
  completion (or its own timeout) even in the rare case where every racing
  caller has also gone away by then — bounded, accepted waste, identical
  tradeoff ADR-019 already made for the clock-persist write.

**Option C: No singleflight coalescing — let every caller hydrate independently.**
- Pros: No shared-context problem, since no context is shared.
- Cons: Directly reopens the exact problem PHASE_2.md Step 3 requires closed:
  redundant concurrent DB round-trips for the same `gameID`, and a real risk
  of two independently-constructed `*GameSession` objects (each with its own
  `Clock` goroutine) for one game — the same double-registration race class
  ADR-017 already closed once for first-connect, reopened here. Not a
  serious contender; rejected outright.

**Decision:** Option B — `context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)`,
scoped to `GetOrHydrate`'s `hydrateFn` invocation only.

**Rationale:**

The correctness requirement is that hydration's success or failure must not
depend on which of possibly-several racing callers happened to be the one
whose call triggered `singleflight.Group.Do`'s single execution — that caller
is an implementation-internal accident, not a meaningful authority over
whether the shared result should exist. Option A makes every piggybacking
caller's outcome hostage to a party they have no relationship with. Option C
solves the context problem by reopening the exact race this method exists to
close. Option B, already established by ADR-019 for the same underlying
reason (an operation whose effect is shared beyond the triggering caller's own
lifecycle must not be tied to that caller's cancellation), is the correct
generalization: this is now the second call site this codebase has needed it
at, reinforcing CLAUDE.md Non-Negotiable Constraint #12 rather than
introducing a new pattern.

**Consequences:**
- `GameRegistry.GetOrHydrate`'s `hydrateFn` call is wrapped in
  `context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)`, called
  out explicitly in a comment at the call site (mirroring ADR-019's own
  Consequences) and in CLAUDE.md's Known Sharp Edges.
- `GetOrHydrate`'s own outer `ctx` parameter (CODING_GUIDELINES.md §2 — every
  I/O function takes context first) is otherwise unused for the actual
  hydration I/O — worth being explicit about so a future reader does not
  assume it's threaded through directly, the same way ADR-019 flagged
  `HandleDisconnect`'s parameter.
- This is the second instance of the ADR-019 pattern (context whose
  cancellation is correct for one caller but wrong once a second concern —
  here, other racing callers — is wired through the same context). Continues
  to reinforce Constraint #12 as a standing question for any new shared or
  asynchronous operation, not just a one-off.
- Step 5 (the resolve endpoint, not yet implemented) is the first real
  caller of `GetOrHydrate` and should not need to reason about this at its
  call site — the detachment is fully encapsulated inside `GetOrHydrate`
  itself.

---

## ADR-028: JoinGame Made a Pure DB Operation; CreateGame Claims Initial
Ownership

**Date:** 2026-07-23
**Status:** ACCEPTED

**Context:**

Real multi-instance E2E testing (Step 11) found `Manager.JoinGame` failing
whenever a `POST /games/:id/join` request landed, via nginx's plain
round-robin, on a different instance than the one that had handled the prior
`POST /games` (`CreateGame`) call. The failure was not intermittent in the
race-condition sense — it was deterministic given which two instances the two
requests happened to land on, close to a 50% rate with two replicas.

Root cause: `JoinGame` read `m.registry.Get(gameID)` — `GameRegistry` is a
plain in-process Go map, private to one running server process's heap.
`CreateGame`'s `m.registry.Register(session)` call had written the session
entry into whichever instance's process memory handled *that* request only.
A different instance's `JoinGame` call has no access to, and no way to
observe, another process's memory — the lookup was not failing due to a
restart or a timing window, it was asking the wrong process for something
that only ever existed in a different process.

This was worse than an ordinary 50%-of-requests failure, for a reason found
only by reading the method in full: `JoinGame`'s `gameStore.UpdatePlayerBlack`
DB write ran and committed *before* the registry lookup. When the lookup then
failed, the method returned a 500 with no token — but the database already
recorded a black player for the game. A retry with a fresh `userID` (as
`get-id-and-token.sh` naturally does on each run) then fails
`UpdatePlayerBlack`'s `player_black_id IS NULL` precondition permanently —
the game is left stuck at `WAITING_FOR_PLAYER`, unjoinable by anyone, with no
black token ever issued to any user. Confirmed directly against live cluster
logs: the DB write succeeded silently, the HTTP response was a 500, and
subsequent join attempts for the same `gameID` returned 409
`GAME_NOT_JOINABLE`.

Separately, but discovered and fixed in the same session: `Manager.CreateGame`
never wrote a Redis ownership record for the game it had just registered
locally. The heartbeat ticker's batched ownership renewal
(`RenewOwnershipBatch`, walking `registry.AllActive()`) then tried, every 3s,
to renew an ownership key that had never been created — its compare-and-swap
correctly failed every time, logging `"heartbeat: lost ownership renewal for
game — no longer the recorded owner"` indefinitely for any game that was
created but never subsequently resolved.

**Options considered for JoinGame:**

**Option A: Give JoinGame the same `GetOrHydrate` fallback `HandleConnect`
received in Step 8.** This was the fix originally diagnosed (from log output
alone, before the full source was re-read in this session) and recorded in
`CLAUDE.md` as the plan going into this session.

- Pros: directly resolves the observed registry-miss error with a pattern
  already proven correct elsewhere in this codebase.
- Cons: rejected on closer reading. `HandleConnect`'s `GetOrHydrate` fallback
  is only safe because `HandleConnect` is exclusively reachable through the
  resolve-then-connect flow (ADR-022) — `ResolveGame` has already claimed
  Redis ownership for this instance *before* minting the `ConnectClaims` that
  points here. `JoinGame` has no equivalent precondition: it is a plain REST
  POST that round-robins with zero ownership awareness. Giving it the same
  fallback would, on any instance other than the true owner, hydrate a
  second, independent, unclaimed `GameSession` object for the same `gameID`
  — reintroducing, through a new code path, exactly the split-brain failure
  mode ADR-021 was written to structurally prevent. It also does not address
  the DB-write/response non-atomicity finding above: a hydrate failure after
  `UpdatePlayerBlack` has already committed reproduces the identical stuck-game
  symptom.

**Option B: Make JoinGame a pure DB operation — remove the registry read and
`GameSession.SetPlayerBlack` call entirely (CHOSEN).**

- Pros: matches this project's own architecture as designed
  (`ARCHITECTURE.md`/`phases/current/PHASE_2.md`: "`POST /games/:id/join` ...
  never touch a live `GameSession` ... no affinity needed, no resolve step
  involved"). Postgres is the one store reachable identically from every
  instance; `UpdatePlayerBlack`'s existing atomic conditional UPDATE
  (ADR-016) is already the full correctness guarantee for "did Black join."
  Grepping every consumer of `GameSession.playerBlackID`
  (`CurrentStateSnapshot`, the WebSocket message protocol in `messages.go`,
  `HandleConnect`'s authorization path) found no live reader — connect-flow
  identity is established entirely from signed JWT claims, never from this
  field. Any `GameSession` later legitimately hydrated fresh from the DB
  (`hydrateGameSession`/`NewGameSessionFromDB`) already reads
  `player_black_id` off the row `JoinGame` wrote, with no extra step needed.
  Removing the registry touch also fully closes the DB/response
  non-atomicity bug: nothing remains after the DB write that can fail the
  request.
- Cons: the specific in-memory `GameSession` object `CreateGame` originally
  built (if still the live one on the owning instance, which it usually is)
  never has its `playerBlackID` field synced after this change — it stays
  `""` for that object's lifetime. Accepted: nothing currently reads it: if a
  future phase needs live-session player identity (rather than DB or JWT
  claims), this should be revisited then, not solved speculatively now —
  consistent with this project's standing discipline against building ahead
  of demonstrated need (ADR-014, ADR-016).

**Decision:** Option B for `JoinGame`. For `CreateGame`: add a best-effort,
non-fatal `ClaimOwnership(ctx, gameID, m.instanceID, "")` call immediately
after `registry.Register(session)`.

**Rationale:**

The deciding factor against Option A was not that it failed to fix the
reported symptom — it would have — but that it reintroduced, via a new call
site, the exact class of bug (an unowned, uncoordinated, independently
hydrated second `GameSession`) that this project's entire Phase 2 design
exists to structurally rule out. A fix that resolves an observed error while
reopening the architecture's central invariant is not an acceptable trade,
regardless of how directly it addresses the symptom in front of it. Option B
is simpler, matches the system's actual required behavior (not merely what a
document says — the documents were re-derived as correct independently, by
checking what actually reads `playerBlackID` and confirming nothing does),
and removes an entire failure mode (the DB/response non-atomicity bug) as a
free side effect rather than requiring a second, separate fix. `CreateGame`'s
ownership claim is a distinct, unrelated-in-mechanism fix bundled into the
same ADR because it was found and fixed in the same session and closes the
other half of the same observed symptom class (heartbeat log spam for
created-but-unresolved games).

**Consequences:**
- `Manager.JoinGame` is now exactly: `GetGame` (precondition checks) →
  `UpdatePlayerBlack` → sign token. No `GameRegistry`/`GameSession` access
  anywhere in the method.
- `Manager.CreateGame` now calls `ClaimOwnership` immediately after
  `registry.Register`, best-effort — logged on failure, never fails game
  creation itself.
- `GameSession.SetPlayerBlack` becomes unused by any production call site.
  Not deleted — it remains directly unit-tested
  (`TestCurrentStateSnapshot_ReflectsSetPlayerBlack` in `session_test.go`,
  unaffected by this ADR) and costs nothing to keep as a general-purpose
  session mutator, but a future session should not be surprised to find it
  has no production caller.
- Three tests updated: `TestManager_JoinGame_UpdatesSessionAndDB` and
  `TestManager_JoinGame_ConcurrentJoins_ExactlyOneWins`
  (`internal/game/manager_test.go`, `manager_race_test.go`) no longer assert
  `session.CurrentStateSnapshot().PlayerBlackID` after a successful join —
  the DB-side assertions, which are the actual invariant ADR-016 protects,
  are unchanged and remain the correctness guarantee.
  `TestManager_JoinGame_RejectsSelfPlay`'s existing empty-`PlayerBlackID`
  assertion still passes (trivially, now) and was left as-is.
- Corrects, rather than implements, the `GetOrHydrate`-fallback approach for
  `JoinGame` that `CLAUDE.md` recorded as the plan at the end of the prior
  session — that approach was never implemented, and is superseded by this
  ADR before any code following it was written.

---

## ADR-029: `ABORTED` Status for a Game Whose Opponent Never Joined

**Date:** 2026-07-24
**Status:** ACCEPTED

**Context:**

Manual E2E testing of Scenario 3 (PHASE_2.md's failover-abandonment test)
surfaced a design flaw predating Phase 2 entirely, not a Phase 2 regression:
`Manager.onAbandonTimeout`, when it fires for a game still in
`WAITING_FOR_PLAYER` (the creator connected, the opponent never joined at
all, and the creator then disconnected), runs the exact same
single/both-disconnected branching built for `ACTIVE` games. `IsPlayerConnected`
for the never-existent opponent returns `false`, which falls into the
both-disconnected branch and attempts `Transition(ABANDONED)` with a `DRAW`
outcome — which cannot even succeed, since `validTransitions` had no
`WAITING→ABANDONED` edge (tracked separately as TD-P2-005), leaving such a
game stuck in `WAITING_FOR_PLAYER` forever. But the deeper problem, caught
during design of TD-P2-005's fix rather than by any test, is that the
intended outcome was wrong regardless: a game where the opponent never showed
up has no meaningful winner (ruling out the single-disconnect "opponent
wins" branch) and was never actually contested (ruling out scoring it a
`DRAW` either). It never started. Real-world chess platforms treat this case
as void, not a drawn game.

**Options considered:**

**Option A: Add the missing `WAITING→ABANDONED` edge, keep the existing
both-disconnected branch's `DRAW` outcome.** Closes TD-P2-005's stuck-forever
bug with a minimal diff.

- Pros: smallest possible change.
- Cons: fixes the state-machine gap but leaves the wrong result recorded —
  a game that never had two contesting players ends up with `outcome: DRAW`,
  identical in shape to a genuine mutual-abandonment draw from an `ACTIVE`
  game. Indistinguishable from a real correctness bug to any future consumer
  of game history (Phase 4's ELO/history work would have no way to tell
  "this was a void, unplayed game" from "these two players drew after both
  disconnecting mid-game" without inspecting move count as an out-of-band
  signal).

**Option B: New `ABORTED` status, no outcome, no outcome_reason (CHOSEN).**
`validTransitions` gains `WAITING→ABORTED`. `onAbandonTimeout` gains an early
branch: if the game is still `WAITING_FOR_PLAYER` when the timer fires,
transition straight to `ABORTED` and skip the ACTIVE-game single/both-disconnected
branching entirely, since that logic is only meaningful for a game that
actually started.

- Pros: correctly represents the actual event — void, not drawn. Matches
  real chess platform conventions the same design principle already leans on
  elsewhere in this project (ADR-015's abandonment-vs-abandoned-draw split
  was reached by the same reasoning: don't let one mechanism paper over two
  genuinely different situations). Self-documenting for any future history/
  ELO feature — an `ABORTED` game is trivially filterable without inspecting
  move count.
- Cons: one new status value, one migration (CHECK constraint), one new
  `validTransitions` edge — marginally more than Option A, but not
  meaningfully more complex to implement.

**Decision:** Option B.

**Rationale:**

This project has consistently preferred correctly modeling a genuinely
different situation over reusing an existing mechanism that happens to be
adjacent (ADR-015 is the direct precedent: single-disconnect vs.
both-disconnected `ACTIVE`-game outcomes were split into `COMPLETED`-with-a-
winner vs. `ABANDONED`-as-a-draw specifically because conflating them produced
a misleading result). A game that never started is not a weaker version of a
drawn game; it's a different event, and Option A's minimal fix would leave it
permanently misrecorded in a way nothing downstream could ever correct after
the fact (unlike a code bug, a wrong `outcome: DRAW` written to a real row is
permanent history).

**Consequences:**
- `store.GameStatusAborted` (`"ABORTED"`) added. `games.status`'s CHECK
  constraint gains it (migration 004).
- `validTransitions[WAITING]` gains `ABORTED: true`.
- `Manager.onAbandonTimeout` branches on `snap.Status == WAITING` before the
  existing `ACTIVE`-game logic; the `ABORTED` branch calls
  `UpdateGameStatus(..., outcome: nil)` — `outcome`/`outcome_reason` stay
  `NULL`, distinct from every other terminal path.
- `GAME_OVER`'s wire payload for this path sends `outcome: ""`,
  `reason: "ABORTED"` — the reason string is wire-only, not routed through
  `store.OutcomeReason` (which has no `ABORTED` member and is not being given
  one, since it isn't a scored result in that taxonomy).
- This also directly closes TD-P2-005, which is now resolved rather than
  merely tracked — the state-machine gap it flagged (`WAITING` had no path
  to any terminal state) no longer exists.
- Bundled with, but independent in mechanism from, ADR-030 below — both were
  found via the same E2E testing session and both concern
  `onAbandonTimeout`, but this ADR is a pure outcome-correctness fix with no
  cross-instance component; a single-instance Phase 1 deployment has exactly
  this same bug today and benefits from this fix identically.

---

## ADR-030: Persisted Disconnect Timestamps for Abandonment-Timer Continuity
Across Failover

**Date:** 2026-07-24
**Status:** ACCEPTED

**Context:**

Manual E2E testing of Scenario 3 found that an `ACTIVE` game's abandonment
timer does not survive an instance failover: if the owning instance dies
while a player is disconnected (mid-60s-grace-period), and the surviving
instance later hydrates the game when the other player reconnects, the game
never reaches an abandonment outcome — it sits `ACTIVE` indefinitely with the
missing player's connection slot permanently empty. Root cause, confirmed
against source: `Manager.abandonTimers` (`map[string]*time.Timer`) is pure
in-process memory, armed via `time.AfterFunc` inside `startAbandonTimer`,
called only from `HandleDisconnect`. `hydrateGameSession` (the function that
rebuilds a `GameSession` on the surviving instance) has no way to know a
disconnect ever happened — nothing persists that fact anywhere — so it never
arms a replacement timer, and `HandleConnect`'s reconnect path only ever
*cancels* a timer for the color that just reconnected, never starts one for
an opponent that's still missing.

This is genuinely new to Phase 2, not a latent Phase 1 bug: in a
single-instance deployment, the process that witnesses a disconnect is
always the same process that would witness the timeout or the reconnect, so
this gap cannot surface. Phase 2 is what introduces a second process that
can inherit a game with zero memory of what the first process saw. This
directly fails `phases/current/PHASE_2.md`'s stated Acceptance Criterion #5
("Scenario 3 ... normal abandonment semantics apply, unmodified") as
written.

**Options considered for where to persist the disconnect fact:**

**Option A: Redis key, mirroring the ownership/liveness pattern (ADR-023).**
A `disconnect:{gameID}:{color}` key, TTL-based.

- Pros: reuses an already-established pattern and store; no new dependency;
  ownership/liveness already prove this project is comfortable reasoning
  about TTL-based ephemeral coordination state in Redis.
- Cons: rejected. Redis's outage is an explicit, documented, *tolerated*
  failure mode in this design — `phases/current/PHASE_2.md`: "Redis going
  down does not crash any instance; live gameplay is unaffected; new
  resolves fail cleanly." Abandonment correctness is a fairness guarantee,
  not a routing concern — it decides who wins or loses a game. If Redis
  happens to be down at the exact moment a takeover instance needs to check
  "was there a pending disconnect, and for how long," a Redis-backed design
  has no way to know at all — indistinguishable from "nobody was ever
  disconnected." That would silently reintroduce this exact bug, just
  conditioned on Redis's health at that instant, which is a materially worse
  failure mode for a fairness-determining fact than for a routing decision.
  Separately: relying on Redis TTL expiry as the enforcement signal (rather
  than just for key cleanup) is ambiguous on its own — an expired key and a
  key that was never set are indistinguishable via a plain `EXISTS` check,
  which would require storing an actual timestamp as the value anyway (at
  which point Redis's TTL machinery isn't even being used for its main
  benefit).

**Option B: Postgres columns, `white_disconnected_at`/`black_disconnected_at`
on `games` (CHOSEN).**

- Pros: Postgres is already unconditionally required for the game to
  function at all — every move and status transition already depends on it —
  so tying fairness-critical state to it introduces no new single point of
  failure, unlike Option A. `hydrateGameSession`/`restoreGame` already fetch
  the `games` row via `GetGame`; two more nullable columns return in that
  *same* query, at no extra round-trip cost — Option A would require a
  mandatory second store round-trip per hydrate. A real timestamp column,
  not a TTL'd key, removes the expired-vs-never-set ambiguity Option A had
  to work around.
- Cons: requires a migration (small, two nullable columns — migration 004,
  bundled with ADR-029's `ABORTED` status change since both were found in
  the same session and touch the same table). Marginally more "chatty" than
  a Redis `SET` per disconnect/reconnect, irrelevant here since this is not
  the move-processing hot path.

**Decision:** Option B.

**Rationale:**

The deciding factor is failure-mode alignment, not raw performance or reuse
of an existing pattern: Redis's documented, accepted failure mode (routing
decisions fail cleanly, gameplay is unaffected) is specifically calibrated
for routing/coordination concerns, not for a fact that determines who wins a
game. Riding on the store that's already unconditionally required removes an
entire class of "what if Redis is down at exactly the wrong moment" reasoning
for this specific piece of state, and does so while being simpler (no
TTL/expiry-ambiguity handling needed) and cheaper (no extra round-trip) than
the Redis alternative would have been.

**Consequences:**
- `games` gains `white_disconnected_at`/`black_disconnected_at` (nullable
  `TIMESTAMPTZ`, migration 004). `store.Game` gains matching fields.
  `GameStore.GetGame`/`GetActiveGames` return them; new
  `GameStore.UpdateDisconnectTimestamp(ctx, id, color, *time.Time)` sets
  (disconnect) or clears (reconnect) one.
- `Manager.HandleDisconnect` persists the disconnect moment (detached-context,
  best-effort, same ADR-019 pattern as its existing clock-persist write) right
  after arming the in-memory timer. `Manager.HandleConnect` clears it
  (best-effort) alongside `cancelAbandonTimer`.
- New `Manager.startAbandonTimerWithDuration` (parameterized duration,
  `startAbandonTimer` becomes a thin wrapper passing the fixed
  `abandonTimeout`), `armAbandonTimerForColor` (computes remaining duration
  from a persisted timestamp, arms via `time.AfterFunc` even at zero
  duration — see its doc comment for why this specifically avoids a
  registration-ordering race against `GetOrHydrate`), and
  `armAbandonTimersForGame` (re-fetches the row and arms both colors; called
  after `GetOrHydrate` returns, not from inside `hydrateGameSession` itself,
  for the same ordering reason).
- Called from `restoreGame` (directly, since it already holds the row and
  registers synchronously) and from both `ResolveGame`'s claim-and-hydrate
  branch and `HandleConnect`'s registry-miss fallback (via
  `armAbandonTimersForGame`, after `GetOrHydrate` returns).
- Not addressed: stale timestamps on a now-terminal game are not explicitly
  cleared (e.g. by `finalizeGame`). Left as harmless dead data —
  `armAbandonTimerForColor`/`onAbandonTimeout`'s own terminal-state check
  (`snap.Status != ACTIVE && != WAITING → return`) means an accidentally
  re-armed timer for a terminal game is a guaranteed no-op, not a
  correctness risk. Worth revisiting only if TD-P2-004's eventual
  session-eviction work ever touches this same area.
- This directly restores `phases/current/PHASE_2.md` Acceptance Criterion #5
  to being actually true, rather than requiring it to be weakened/descoped.

---

## ADR-031: `NULL`-Ambiguity Fix — Assume-Disconnected-Now Fallback When No
Disconnect Was Ever Individually Observed

**Date:** 2026-07-25
**Status:** ACCEPTED

**Context:**

An independent design review (a separate session, deliberately briefed
neutrally rather than asked to validate ADR-030) found a real gap in
ADR-030's implementation, confirmed against source before acting on it here:
`white_disconnected_at`/`black_disconnected_at` are only ever written from
inside `Manager.HandleDisconnect` — which only runs when *this process's own
code* observes a connection close. If the owning instance itself dies while
both players are still actively connected (a hard kill, an OOM, a crash, or
a graceful shutdown that doesn't finish routing every connection through
`ws.Registry.CloseAll`'s close path before a forced kill arrives), **neither
timestamp is ever written** — not "expired," never set at all. When a
survivor later hydrates the game, `armAbandonTimerForColor` treats `NULL` as
"no grace period needed" (correct in ordinary steady state) and arms
nothing for either color. If one player then reconnects and the other never
does, no timer exists anywhere to eventually resolve it — the game sits
`ACTIVE` forever. This is functionally the same failure ADR-030 exists to
close, re-entered through a path ADR-030's original design didn't account
for: **the instance-death case, not the individually-observed-disconnect
case**, which by nature of how and when instances actually die in the
Scenario 1–3 walkthrough (`docker compose stop`/`kill`) may be the *more*
common trigger, not an edge case.

Root cause, stated precisely: `NULL` was being used to mean two different
things — "definitely fine" and "unknown, because the process that would
have recorded a disconnect died first." This is the exact ambiguity class
ADR-030 explicitly rejected when it ruled out a bare Redis `EXISTS` check
("an expired key and a key that was never set are indistinguishable"),
reintroduced one layer up in the chosen Postgres design because a `NULL`
column has the identical two-meanings problem a TTL-expired key does.

**Options considered:**

**Option A: Fresh full `abandonTimeout` for any not-yet-reconnected color at
hydration, ignoring any real elapsed time.**

- Pros: simplest possible fix, no need to distinguish the two `NULL`
  meanings at all.
- Cons: rejected. Discards real information in the case where elapsed time
  actually is known (a genuinely individually-observed disconnect) would
  never reach this branch anyway (persisted is non-nil there), so this
  option only matters for the crash case — but even there, it's strictly
  worse than Option B for no benefit: it's never more correct, only ever
  equal to or more generous than necessary in a way that's harder to reason
  about (why would an assumed-fresh 60s be right if the crash happened,
  say, 3 minutes before anyone noticed?).

**Option B: Deriving the fallback timestamp from the routing directory's
last liveness renewal instead of `time.Now()`.**

- Pros: could in principle produce a tighter estimate of the actual crash
  moment than "whenever someone happens to reconnect."
- Cons: rejected. Redis's liveness key (`instance_alive:{instanceID}`, ADR-023)
  is renewed every 3s specifically for fast failure detection, not retained
  history — by the time a survivor is hydrating this game, the dead
  instance's liveness key has already expired and is gone; there is nothing
  left to read a "last renewal time" from. Reconstructing this would require
  persisting additional liveness history nowhere else in this design keeps,
  for a fairness benefit that only ever helps in the direction of being
  *less* generous to the disconnected player — the wrong direction to add
  complexity for.

**Option C: Reactive arming only — arm the opponent's timer only once the
first player's `HandleConnect` actually succeeds, never at hydration time
itself.**

- Pros: avoids arming a timer for a game nobody has touched yet; slightly
  less speculative than arming both colors unconditionally at hydration.
- Cons: rejected as incomplete, not wrong. Leaves a real case unresolved: a
  player who calls `/resolve` (successfully claiming/hydrating the session)
  but never completes the WebSocket dial within `ConnectClaimsTTL` — under
  Option C, nothing would ever arm a timer for the *other* color in that
  scenario, since no `HandleConnect` ever ran on this instance to trigger
  it.

**Option D: Periodic sweep/poll across all locally-registered `ACTIVE`/
`WAITING_FOR_PLAYER` sessions with stale/missing connections, instead of
timer reconstruction at hydration (CHOSEN alternative rejected, see
Decision).**

- Pros: structurally more robust — doesn't depend on hydration being the
  only trigger point; would also close the "nobody ever resolves again"
  gap (see Consequences) that no hydration-triggered fix can close by
  construction.
- Cons: rejected for this fix, though not because it's wrong — it's
  disproportionate for what is otherwise a small, localized correction, and
  this project has repeatedly declined exactly this shape of
  build-ahead-of-need complexity (ADR-014, ADR-016, ADR-023's own
  TD-P2-001/003 deferrals). Recorded as a candidate design for the
  "nobody ever resolves" gap below if it's ever shown to matter in
  practice, not dismissed outright.

**Decision:** `effectiveDisconnectedAt(status, persisted)` — a fallback
applied only at hydration (`armAbandonTimersForGame`, `restoreGame`), never
from `HandleDisconnect` itself: if a persisted timestamp exists, trust it
as-is (unchanged from ADR-030); if it's `nil` and the game is `ACTIVE` or
`WAITING_FOR_PLAYER`, assume disconnected as of `time.Now()` rather than
assuming fine.

**Rationale:**

A freshly-hydrated `GameSession` always starts with both connection slots
empty regardless of *why* — individually-observed disconnect, instance
death, or genuinely nobody has connected yet — so hydration time is the one
point where the ambiguity is unavoidable and must be resolved one way or the
other. Erring toward "assume disconnected" costs nothing in the case where
the assumption is wrong: a player who is in fact fine and reconnects moments
later self-cancels this defensively-armed timer through the exact same
`cancelAbandonTimer`/`UpdateDisconnectTimestamp(nil)` path that already
cancels a real disconnect timer (unchanged, ADR-030). The cost of guessing
wrong in this direction is a few seconds of a timer that gets cancelled
before it matters — the cost of guessing wrong in the other direction (the
original bug) is a game stuck `ACTIVE` forever. This asymmetry is why
"assume disconnected" and not "assume fine" is the only defensible default.

**Consequences:**
- New package-level helper `effectiveDisconnectedAt(status store.GameStatus,
  persisted *time.Time) *time.Time` in `internal/game/manager.go`. Called
  from `armAbandonTimersForGame` and `restoreGame`, wrapping the value
  passed to `armAbandonTimerForColor` — `armAbandonTimerForColor`,
  `onAbandonTimeout`'s branching, the `ABORTED` branch, and `finalizeGame`
  are all unchanged; this is additive, not a rewrite of anything ADR-029/030
  already got right.
- Explicitly NOT closed by this fix, and cannot be by construction: a game
  where **nobody ever calls `/resolve` again** after an instance dies.
  Nothing runs if nobody triggers a hydrate — `effectiveDisconnectedAt` only
  ever executes at the moment someone does. Tracked as new technical debt
  (not scheduled, no demonstrated need yet — candidate design if ever
  needed: a periodic sweep, Option D above, folded into the existing
  heartbeat tick rather than a new ticker).
- TD-P2-001's existing false-positive liveness window gains a compounding
  note: under that window, a competing phantom session hydrated on a second
  instance while the original owner is in fact still alive would, as of
  this fix, also arm defensive abandon timers via `effectiveDisconnectedAt`
  for a game that was never actually down — same bounded window TD-P2-001
  already documents, now with an additional possible symptom (a spurious
  abandon-loss for a genuinely-connected player) alongside the
  already-documented duplicate-processing risk. Not a new risk class, an
  addition to an already-accepted one.
- Regression tests required (not yet written as of this ADR): a
  two-`Manager`-instance test proving the crash-while-both-connected case
  now resolves correctly (previously unclosed), and a test confirming the
  individually-observed-disconnect case (already correct under ADR-030
  alone) does not regress — a single-`Manager` test cannot reproduce either,
  per this project's own established Known Sharp Edges note on this class of
  bug.

---
