# Architecture Decision Records — Phase 3

Continuation of `DECISIONS_LOG_PHASE_2.md`'s numbering (ADR-032 onward) in a
separate file, split by phase for readability. Numbering is global and
sequential across all three files — there is exactly one ADR-032 in this
project, and it lives here. Same format, same append-only discipline: entries
are never edited after acceptance; reversals get a new ADR that supersedes the
old one by reference.

**Note on prior Phase 3 work:** a Phase 3 pre-planning attempt prior to
2026-08-06 (would-have-been ADR-032 through ADR-036, an SSE+Redis-pub/sub/
Lua-pairing design, `ActiveGameIndex`, and associated edits to `PHASE_3.md`)
was discarded in full on 2026-08-06 for hallucinated content never actually
implemented or reviewed. None of it was ever written to this file — no
renumbering or superseding entry is needed, since nothing under these numbers
existed here before now. The ADRs below are a from-scratch design pass.

Full design reasoning, mechanism-level detail (exact Redis key/value shapes,
JSON payload examples, nginx config specifics) lives in
`phases/current/PHASE_3_DESIGN_NOTES.md`, referenced by section (`§N`) below
rather than reproduced — these entries record the decisions and why, not
every supporting detail.

---

## ADR-032: Matchmaking Topology and Pairing Model — Separate Service, Server-Picks

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

`PHASE_3.md`'s Scope section states matchmaking will run as an in-process
goroutine as settled fact, while its own "Architecture Decision Required"
section, two sections later, treats the identical question as open and gates
Phase 3 start on writing this ADR — a direct internal inconsistency, confirmed
by direct read of the document rather than trusting either half.

A pre-implementation audit found matched-game creation cannot safely be
`CreateGame`+`JoinGame` chained — two sequential writes reopen the
"incompletely assigned game" failure class `DECISIONS_LOG_PHASE_2.md` ADR-028
already closed once for the shared-link path. It requires a single atomic DB
insert assigning both players in one statement (confirmed against
`internal/store/game_store.go`: `games.player_black_id` is already a plain
nullable column, so this needs one new `GameStore` method; no migration
required for the insert itself).

Separately, tracing SSE notification delivery (required regardless of
topology — polling was rejected as the primary mechanism, see ADR-036)
surfaced a structural problem: a player's match-notification connection is
sticky to whichever process accepts it. If pairing happens inside
chess-server, any of N instances could pair a match while either player's
notification connection sits on a *different* one of N instances — an N×N
delivery problem chess-server, built around single-owner/no-fan-out state
(`DECISIONS_LOG_PHASE_2.md` ADR-021), has no good place to absorb.

**Options considered:**

**Option A: In-process goroutine inside chess-server, no package isolation** —
matches `PHASE_3.md`'s original Scope wording exactly.
- Pros: no new deployment artifact; `CreateMatchedGame` mirrors `CreateGame`'s
  eager local-register + ownership-claim pattern with zero network hop for
  game creation itself.
- Cons: does not solve the SSE N×N delivery problem above — that problem
  exists independently of how matchmaking logic is packaged, and in-process
  gives chess-server no clean place to absorb it. No discipline preventing
  matchmaking logic from entangling with `game.Manager` internals over time.

**Option B: Separate service, service-assigns.** matchmaking-service owns
pairing and assigns each match to a chess-server instance — either
round-robin/least-loaded (requires new live instance-membership and load
tracking, neither of which exists in this codebase today; the LB's static
map is the closest analog and is deliberately mechanical/dumb by design,
ADR-022), or broadcast-and-race (fan a candidate pair out to every instance,
first `ClaimOwnership` CAS win takes it — reuses existing first-claim-wins
semantics for free, but pays an N-instance RPC fan-out on every single match
for zero correctness benefit over Option D).
- Also considered under this option: a persistent bidirectional gRPC stream
  per chess-server instance, matchmaking-service pushing assignments over it
  (streaming service-assigns) — a genuinely different RPC pattern (streaming
  vs. unary) than ADR-033's eventual choice, worth naming since it was a live
  alternative. Rejected: trades broadcast-and-race's per-match fan-out cost
  for persistent-connection lifecycle management (reconnect/backoff) to avoid
  a cost Option D already avoids without needing either.
- Cons (all sub-variants): none solve anything Option D doesn't already solve
  more simply; all add new coordination machinery Option D has no need for.

**Option C: In-process goroutine, isolated in `internal/matchmaking` package.**
Option A implemented with more discipline (package boundary makes future
extraction mechanical rather than a redesign) — does not address the SSE
delivery problem either, since that problem is about process topology, not
code organization.

**Option D: Separate service, server-picks (CHOSEN).** Each chess-server
instance runs its own pairing loop against the shared Redis queue
(`ZPOPMIN queue 2`, atomic — Redis guarantees exactly one instance wins any
given pair). matchmaking-service has no role in the pairing *decision* — only
queue intake, SSE relay, and gRPC report receiving (`PHASE_3_DESIGN_NOTES.md`
§4).
- Pros: collapses the SSE N×N problem to at most M×M within
  matchmaking-service's own replica set (M starts at 1) — chess-server never
  needs to know which matchmaking-service replica holds a given SSE stream,
  one RPC call resolves it. No chess-server-to-chess-server communication
  exists anywhere in this design. Requires no new coordination/service-
  discovery machinery — atomicity of `ZPOPMIN` *is* the entire pairing
  mechanism.
- Cons: matchmaking-service now needs a narrow gRPC-receiving surface
  (ADR-033) that Option A/C wouldn't have needed at all — accepted, since it
  replaces a harder, unsolved problem (SSE fan-out) rather than adding a new
  one.

**Decision:** Option D.

**Rationale:**

The deciding factor is the SSE delivery problem, not the matchmaking logic
itself — no amount of package discipline (Option C) fixes a process-topology
problem. Extraction collapses N×N to at most M×M and eliminates
chess-server-to-chess-server communication by construction, matching this
project's demonstrated preference (`DECISIONS_LOG_PHASE_2.md` ADR-021, ADR-022)
for architectures where a class of bug is structurally impossible rather than
carefully avoided. Server-picks (over service-assigns) needs no new
coordination machinery: `ZPOPMIN`'s atomicity already provides the "exactly
one owner" guarantee service-assigns would otherwise have to construct via
round-robin/load-tracking or a broadcast race.

**Consequences:**
- `internal/matchmaking` package (chess-server side) owns the pairing loop
  and `Manager.CreateMatchedGame` (new: atomic two-player DB insert, local
  `GameRegistry` registration, `ClaimOwnership` — mirrors `CreateGame`'s
  existing eager pattern, deliberately not deferred to `GetOrHydrate`, to stay
  behaviorally consistent with shared-link game creation and avoid
  reintroducing ADR-028's heartbeat-renewal-spam class for a newly-common
  case).
- matchmaking-service is a new deployable, own container, own `docker-compose.yml`
  entry, sharing chess-server's existing nginx edge proxy (ADR-035) and Redis
  instance (new namespace, not new infrastructure).
- Pairing-loop timer interval: fixed, configurable via env var
  (`MATCHMAKING_PAIRING_INTERVAL_MS`), no config file — consistent with this
  project's existing plain-env-var convention (`.env.example`,
  `docker-compose.yml`'s `INSTANCE_ID`).
- Concrete extraction-reconsideration trigger recorded, not scheduled:
  revisit if queue/pairing latency is shown to degrade independently of
  active-game count, or a future phase's matching requirements (ELO
  weighting, cross-region) demand independent scaling from game-serving.
- `PHASE_3.md`'s Scope section and "Architecture Decision Required" section
  need to be reconciled to this decision in the same pass this ADR is
  accepted, per this project's standing documentation-integration discipline.

---

## ADR-033: Match-Report RPC Contract, Reliability, and Trust Boundary

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

ADR-032 requires chess-server to report a pairing outcome to matchmaking-service
after the fact — matchmaking-service has no role in the decision itself, only
in relaying its result. Full contract detail: `PHASE_3_DESIGN_NOTES.md` §6–§7.

**Options considered:**

**Option A: Streaming RPC.** Rejected in ADR-032's context already — no
per-call setup-cost benefit at this event rate (one report per match, not a
hot loop); the persistent-connection channel already exists via gRPC's
underlying HTTP/2 connection regardless of unary vs. streaming.

**Option B: One RPC method with a success/failure `oneof`.**
- Pros: fewer methods.
- Cons: the two outcomes share almost no fields and have completely different
  downstream handling (dual SSE push with connect info vs. a single failure
  hint) — a `oneof` mostly produces a message where half the fields are
  meaningless depending on branch.

**Option C: Two unary RPC methods, `ReportMatchCreated`/
`ReportMatchmakingFailed`, fully-populated messages, enum (not string) for
failure reason (CHOSEN).**
- Pros: each message fully meaningful on its own; enum reason is extensible
  without a field-shape change later (validated by ADR-038, which needed
  exactly that extensibility) and matches `CODING_GUIDELINES.md`'s
  forbidden-pattern rule against hardcoded strings for state, applied at a
  service boundary rather than relaxed there.
- Cons: none identified.

**Decision:** Option C. Field named `instance_label` (not `instance_id`) —
confirmed against `internal/game/resolve.go` and `nginx.conf` as the actual
existing convention (`auth.ConnectClaims.InstanceLabel`,
`Manager.ResolveGame`'s return value, nginx's `$instance_label`), not a new
name for the same concept.

**Rationale:**

`ReportMatchmakingFailed` is scoped narrowly and deliberately: it fires only
for a confirmed, terminal failure of `CreateMatchedGame`'s atomic insert after
retries are exhausted — mirroring `CreateGame`'s existing failure semantics
(`DECISIONS_LOG_PHASE_2.md` ADR-028), where a failed `ClaimOwnership` is
separately already non-fatal/best-effort. `CreateMatchedGame` must not deviate
from that established meaning of "best-effort" for the same underlying
operation shape.

Trust boundary: shared secret via gRPC call metadata, checked by a
server-side interceptor — chosen over network policy specifically because
this is Docker Compose, not Kubernetes. Compose has no declarative
per-service network-policy primitive equivalent to a K8s `NetworkPolicy`; any
container on the same Compose network reaches any other by default, so
"network policy" is not an available mechanism here, only an aspirational
one. Documented as a deliberate interim choice, to be replaced with mTLS
post-Phase-3, not solved now.

**Consequences:**
- `.proto` lives in a shared `proto/` directory in this same repo (monorepo,
  not a separate proto module) — both chess-server and matchmaking-service
  are Go.
- `game_id` is the idempotency key for `ReportMatchCreated` — matchmaking-service
  keeps a short-TTL Redis dedup key (`reported:<gameID>`) checked before
  pushing SSE, covering a chess-server retry after a lost ACK.
  `ReportMatchmakingFailed` has no natural key (no game exists yet); accepted
  at-least-once without dedup infrastructure.
- chess-server retries the RPC itself, bounded, with backoff. The game
  already exists in Postgres by the time either RPC fires — a failed report
  must never roll back or retry game creation, same shape as `CreateGame`'s
  ownership claim and `HandleDisconnect`'s timestamp persist
  (`DECISIONS_LOG_PHASE_2.md` ADR-030): log and move on if retries exhaust.
- If the RPC never succeeds at all: falls back to `GET /matchmaking/status`
  (ADR-036) — client polls, sees the game exists via the same mechanism
  ADR-037 relies on, same backstop pattern as SSE-vs-polling elsewhere in
  this design.

---

## ADR-034: Re-enqueue Ownership — chess-server Direct, With Insert-Layer Idempotency

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

If `CreateMatchedGame`'s atomic insert fails after `ZPOPMIN` has already
removed both players from the queue, they need to either be re-enqueued or
told to retry manually. Two owners were considered for this action:
chess-server (direct Redis write, no RPC) or matchmaking-service (via an RPC
report first).

**Options considered:**

**Option A: matchmaking-service owns re-enqueue.** Requires
`ReportInsertFailed`-shaped RPC on every transient failure, not just the
terminal case ADR-033 scoped `ReportMatchmakingFailed` to.
- Pros: keeps all queue-mutation logic in one service.
- Cons: increases RPC volume on every transient failure with no offsetting
  correctness benefit once Option B's fix is in place (see Rationale) —
  raised as a direct concern during design and confirmed valid.

**Option B: chess-server owns re-enqueue directly (CHOSEN).** One Redis
`ZADD`, no RPC, for the recoverable case; `ReportMatchmakingFailed` (ADR-033)
fires only for the terminal, retries-exhausted case.
- Pros: fewer RPCs; `ZPOPMIN`'s atomicity already means no other instance can
  touch a popped pair while they're out of the queue, and `ZADD` re-adding a
  member is naturally idempotent (sorted-set members are unique — re-adding
  just updates the score), so even an accidental double re-enqueue is safe by
  construction for the *clean*-failure case.
- Cons: the clean-failure case was not actually the hazard — see Rationale.

**Decision:** Option B, gated on a new prerequisite: a `matchmaking_request_id`
(`UUID`, `UNIQUE`) column on `games`, generated once by chess-server per
pairing attempt (right after the winning `ZPOPMIN`), reused across all
retries of that attempt, insert issued with
`ON CONFLICT (matchmaking_request_id) DO NOTHING`.

**Rationale:**

Tracing the re-enqueue question surfaced the actual hazard, which is not
where it was initially assumed to be: a clean, unambiguous DB error carries
no race (argued above). The real risk is an *ambiguous* failure — chess-server's
insert attempt times out or the ACK is lost, and it cannot tell whether the
`INSERT` actually committed before declaring failure and re-enqueuing. If it
re-enqueues in that state and the insert had in fact succeeded, both players
end up matched to a live game *and* back in the queue — a double-booking.
This risk is orthogonal to which component performs the re-enqueue;
matchmaking-service re-enqueuing on chess-server's say-so inherits the
identical risk, since it can only act on whatever ambiguous signal
chess-server reports. The `matchmaking_request_id` idempotency key closes
this at its actual source (the insert itself), making retries safe regardless
of ACK loss — failure is only ever declared once genuinely confirmed. Once
that's true, Option A's extra RPC volume has no remaining correctness benefit
to justify it. Use `UUID` v4: confirmed against `internal/store/game_store.go`,
whose `CreateGame` doc comment states `game.ID (UUID v4)` explicitly —
correcting an earlier working assumption in this design pass that this
codebase used UUID v7 for DB primary keys; the actual, current practice is v4
throughout, per source, not per any prior assumption.

**Consequences:**
- Migration: `matchmaking_request_id UUID UNIQUE` on `games` (nullable —
  only matched games populate it).
- `CreateMatchedGame`'s insert becomes idempotent under retry; a bounded
  retry count is enforced before falling back to re-enqueue-with-original-score
  or, if retries are exhausted, `ReportMatchmakingFailed` (ADR-033).
- Re-enqueue preserves the original enqueue score/timestamp (fairness — a
  player should not lose queue position for a server-side failure that
  wasn't theirs), not a fresh `ZADD` timestamp.

---

## ADR-035: matchmaking-service Shares the Existing nginx Edge Proxy; No New Connect-Info Field

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

A concern was raised that a chess-server instance reachable by a different
IP/domain shouldn't break connect-info delivery — worth taking seriously,
confirmed real by reading `nginx.conf` and `docker-compose.yml` directly:
`/connect/{instanceLabel}` is currently a **relative path**, safe only
because the client is already talking to nginx's single public origin for
both the REST API and the WS upgrade. `nginx.conf` has **no location block
for matchmaking-service at all** today — its origin is a genuinely open
question, not just a field-shape question.

**Options considered:**

**Option A: matchmaking-service gets a separate origin/port.**
- Pros: independent exposure, no changes to the existing edge proxy.
- Cons: a bare relative path delivered over matchmaking-service's own SSE
  stream becomes ambiguous to the client (relative to which host?) —
  `instance_label` alone would stop being sufficient, and a fully-qualified
  URL field would become necessary in every connect-info payload
  (`ReportMatchCreated`, the SSE payload, and ADR-037's cached marker),
  adding a field whose only job is compensating for a routing choice made
  elsewhere.

**Option B: matchmaking-service shares the existing nginx edge proxy's
single public origin, via a new `location /matchmaking { ... }` block
(CHOSEN).**
- Pros: reuses machinery that already exists for exactly this job rather
  than standing up a second one; the existing relative-path convention
  (`instance_label` → `/connect/{instanceLabel}`) keeps working unmodified
  for SSE-delivered connect info, identical to how `/resolve` already works
  today. No `connect_url`/`connect_path` field needed anywhere in this
  design.
- Cons: the new location block needs SSE-specific nginx config
  (`proxy_buffering off`, extended `proxy_read_timeout`) — see ADR-036.

**Decision:** Option B.

**Rationale:**

The concern that prompted this ADR was valid and pointed at a real
unresolved question (matchmaking-service's origin), but the fix is a routing
decision, not a new field. Extending the proxy that already does exactly
this job for chess-server is less new surface than standing up and
maintaining a second one, and it preserves a single-origin invariant for the
whole client-facing system rather than fragmenting it.

**Consequences:**
- `nginx.conf` gains a `location /matchmaking` block routing to
  matchmaking-service's container, alongside the existing `chess_rest` and
  `/connect` blocks.
- Every connect-info payload in this design (`ReportMatchCreated`, ADR-036's
  SSE/status payloads, ADR-037's cached marker) carries `instance_label` and
  a precomputed relative `wsPath`, never an absolute URL. This is a load-bearing
  invariant of this design: if matchmaking-service is ever moved off this
  shared origin, every one of those payload shapes needs revisiting, not just
  this ADR.

---

## ADR-036: Matchmaking SSE Notification Design

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

Polling was rejected as the primary match-notification mechanism (this was
the entire reason ADR-032 chose extraction in the first place — see that
ADR's Context). SSE requires its own token model (`PlayerClaims` doesn't fit
— it needs a `GameID` that doesn't exist yet at queue-time), its own
endpoints, and nginx configuration the existing WS block's settings don't
automatically cover. Full detail: `PHASE_3_DESIGN_NOTES.md` §11, §15.

**Options considered for the token:**

**Option A: Reuse `PlayerClaims` early, minted at enqueue time with a
placeholder `GameID`.**
- Cons: rejected outright — `PlayerClaims` is a per-game credential by
  design; inventing a placeholder `GameID` for a game that doesn't exist yet
  corrupts its meaning for every other consumer of that claims type.

**Option B: New `MatchmakingClaims{UserID}` type, same signing secret, short
TTL mirroring `ConnectClaimsTTL`'s pattern (CHOSEN).**
- Pros: matches this codebase's one-secret/multiple-shapes convention
  (`token.go`); scopes the SSE stream to a specific `userID`, closing the
  gap where anyone who knows/guesses a `userID` could otherwise open a
  stream and intercept another player's eventual `PlayerClaims`. Verified
  once at connection open, never re-checked for the stream's life — same
  model as `ConnectClaims` at WS upgrade.

**Decision:** Option B, plus:
- `POST /matchmaking/queue` (mint + `ZADD queue NX`, idempotent — a second
  call for an already-queued player doesn't reset their position, but still
  returns a fresh token) and `GET /matchmaking/stream?token=...` (SSE) as two
  separate endpoints, not combined — combining them would make an SSE
  reconnect re-run the enqueue side-effect, resetting queue position on every
  network drop.
- `GET /matchmaking/status?token=...` as the polling/backstop endpoint,
  serving both a browser/proxy fallback and the lost-RPC/lost-SSE backstop
  (ADR-033's Consequences) with one mechanism, not two.
- `DELETE /matchmaking/queue` for voluntary cancel (`ZREM`), found missing
  during closure of the queue-starvation question below.

**Rationale:**

Splitting mint-and-enqueue from the SSE connection itself is not simply
mirroring resolve-then-connect for consistency's sake — it prevents a real
bug: a combined endpoint's first action on reconnect would be enqueuing
again. `GET /matchmaking/status` reusing the exact backstop role already
established for lost RPC delivery (ADR-033) rather than inventing a second
fallback mechanism follows this project's standing preference for one
correct path over topology-specific ones.

Queue-starvation (closed in this same pass): bounded wait, not indefinite.
matchmaking-service runs its own periodic sweep — separate ticker from
chess-server's pairing-loop ticker (ADR-032), reusing the "one loop
appended-to, not a new worker per fact" instinct ADR-037 also relies on —
scanning the queue ZSET for members older than
`MATCHMAKING_QUEUE_TIMEOUT_SECONDS` (configurable, same convention as
ADR-032's pairing interval), removing them, and pushing `MATCHMAKING_FAILED`
with `reason: QUEUE_TIMEOUT`. This surfaced a precise scoping question for
`MatchmakingFailureReason` (ADR-033): a queue timeout never touches gRPC —
chess-server never attempted a pairing for a timed-out player, so it has
nothing to report. The proto enum stays scoped to what chess-server can
actually experience (`RETRIES_EXHAUSTED` only); matchmaking-service needs its
own, broader client-facing reason vocabulary (`RETRIES_EXHAUSTED`, translated
from the gRPC enum, plus `QUEUE_TIMEOUT`, generated natively). This is
exactly the case ADR-033's enum-not-bool choice anticipated.

**Consequences:**
- `internal/auth` (or matchmaking-service's own auth package, monorepo per
  ADR-033) gains `MatchmakingClaims`/`MatchmakingClaimsTTL` (60s, mirroring
  `ConnectClaimsTTL`).
- New `location /matchmaking` block (ADR-035) requires `proxy_buffering off`
  (nginx's default response buffering silently breaks SSE's real-time
  delivery) and the same extended `proxy_read_timeout` (3600s) the existing
  WS block already carries. Server-side SSE keepalive comments cover
  anything in between. No new library — stdlib `http.Flusher` + chi.
- SSE payload shapes use `event:` as the type discriminator (SSE's native
  mechanism); `GET /matchmaking/status` uses an explicit `status` field
  (no equivalent native mechanism for plain JSON). Both mirror `/resolve`'s
  existing field names (`connectToken`, `instanceLabel`, `wsPath`, confirmed
  against `resolveResponseData` in `resolve_test.go`) rather than the
  proto's snake_case — two separate wire contracts (gRPC internal,
  JSON client-facing) that should not have shared a casing convention by
  accident. Exact payload examples: `PHASE_3_DESIGN_NOTES.md` §15.
- Scaling caveat, accepted, not solved now: matchmaking-service's SSE
  connections live in an in-memory map (`userID → flusher`), correct at one
  replica. If matchmaking-service is ever scaled beyond one replica, it
  inherits the identical structural problem ADR-032 extracted matchmaking
  logic out of chess-server to avoid, at a smaller scale. Not a concern at
  M=1 (premature to solve now); on record so it isn't a surprise later.

---

## ADR-037: Check-Before-Enqueue via a Heartbeat-Renewed Active-Game Marker, Not a Cross-Service DB Query or an Explicit-Clear Cache

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

Nothing in ADR-032/034/036 prevents a player who already has an active game —
matchmaking-originated *or* the existing shared-link `CreateGame`/`JoinGame`
flow — from being silently re-enqueued, nor tells an already-*queued* player
they're already waiting. Confirmed as needing to cover both origins, not just
matchmaking-originated games: a player mid-game via a shared link should not
also be able to queue for a random match. Full detail: `PHASE_3_DESIGN_NOTES.md`
§13.

**Options considered:**

**Option A: Redis cache with a long TTL (e.g. 1h), explicit clear on game
completion, plus a background reconciliation worker to catch missed clears.**
- Pros: keeps matchmaking-service off Postgres.
- Cons: rejected. A mechanism that needs its own worker to correct itself
  for drift is evidence of duplicating a source of truth that already exists
  elsewhere (`games.status`) rather than being one — the same reasoning this
  project has already applied twice (`DECISIONS_LOG_PHASE_2.md` ADR-021's
  Postgres-authoritative/Redis-ephemeral split, ADR-024's rejection of
  duplicating a guarantee at a second trigger point). An explicit-delete
  design's failure mode (a missed delete lingers until the worker notices) is
  strictly worse than what Option C below achieves for free.

**Option B: A new stateless REST endpoint on chess-server
(`GET /players/{userID}/active-game`), queried synchronously by
matchmaking-service on every `POST /matchmaking/queue` call.**
- Pros: Postgres remains the sole source of truth, no cache to go stale, no
  reconciliation worker. Reuses `Manager.ResolveGame` entirely for the
  found-a-game branch, rather than inventing a second token-minting path.
- Cons: rejected — raised directly during design as an unacceptable added
  network hop and DB round-trip on a path that needs an immediate,
  low-latency answer, particularly under repeated client retries. Correct
  in principle (matches ADR-021/024's Postgres-authoritative reasoning) but
  wrong on the latency requirement specifically.

**Option C: A Redis key, written and renewed by chess-server, read-only for
matchmaking-service, using a heartbeat/lease pattern rather than
explicit-clear (CHOSEN).**
- Pros: satisfies the latency requirement Option B failed (one Redis read,
  no HTTP hop, no DB call, on the same connection matchmaking-service already
  holds for the queue) *without* Option A's self-healing failure — there is
  no delete to miss, because there is no delete: the owning instance either
  keeps renewing (game still active) or stops (game ended, or the instance
  died), and the entry ages out within one TTL window either way. This is the
  identical pattern `directory.go`'s `OwnershipTTL`/`OwnershipRenewInterval`
  already uses, tested and working, for the ownership record itself.
- Cons: chess-server gains new write responsibility (a per-player Redis key,
  in addition to the existing per-game ownership key) — accepted, since it's
  appended to a heartbeat loop that already exists and already iterates this
  exact data (locally-owned active games), not a new goroutine.

**"Is this user already queued" — answered separately, not merged into
Option C's key.** The queue ZSET (`ZSCORE queue {userID}`) already answers
this with zero new writes; it is not a copy of the truth, it *is* the truth.
A proposal to merge "queued" and "active game" into one key with a `status`
enum was considered and rejected: "queued" would need its own new writer
(matchmaking-service) and its own new renewal loop to avoid going stale
independently of Option C's game-derived renewal — new machinery to answer a
question `ZSCORE` already answers for free.

**Decision:** Option C.

**Rationale:**

The deciding factor is that Option A and Option C answer the same question
with structurally different failure modes despite superficial similarity
(both are "a Redis key with a TTL") — only Option C's periodic-renewal shape
is genuinely self-healing; Option A's explicit-delete shape is not, which is
precisely why it needed a worker Option C does not. Option B was
directionally correct (Postgres as sole source of truth) but failed a
concrete, stated latency requirement; Option C achieves the same
no-second-source-of-truth property Option B was reaching for, differently —
by deriving the Redis fact directly and continuously from the process that
actually knows it (the owning instance's in-memory registry), rather than by
querying Postgres synchronously per request.

**Consequences:**
- Key: `matchmaking_active_game:{userID}` — deliberately not `game:*`, which
  already names the ownership key (`game:{gameID}`) in `directory.go`;
  reusing that prefix for a userID-keyed, opposite-direction mapping would be
  ambiguous in `redis-cli KEYS 'game:*'` later.
- Value: `{gameID, connectToken, instanceLabel, wsPath}` — `connectToken` is
  the player's existing `PlayerClaims` (24h TTL per `token.go`'s
  `SignPlayerToken` doc comment, confirmed safe to mint once and republish
  unchanged on every renewal with no staleness risk at any relevant
  timescale). Same field names as ADR-036's SSE/status payloads —
  intentional, so a hit here returns the identical shape a normal
  `MATCH_FOUND` event would.
- Scope: every locally-owned game with status `WAITING_FOR_PLAYER` or
  `ACTIVE` — the same set `idx_games_status`'s partial index already covers,
  confirmed via that index's own doc comment: *"used by `GetActiveGames` on
  server restart and by the matchmaking sanity checks added in later
  phases"* — this mechanism was anticipated at schema-design time, not
  invented from nothing here.
- Write triggers: created eagerly and synchronously at `CreateGame`,
  `JoinGame`, and `CreateMatchedGame` success (closing an
  otherwise-`OwnershipRenewInterval`-wide gap between creation and the first
  heartbeat tick); renewed every `OwnershipRenewInterval` (10s) by the same
  per-tick loop that already batch-renews ownership keys via
  `RenewOwnershipBatch`; TTL `OwnershipTTL` (30s) — reused constants, not new
  numbers, matching `DECISIONS_LOG_PHASE_2.md` ADR-023's existing margin
  reasoning. No explicit delete anywhere.
- `POST /matchmaking/queue` and `GET /matchmaking/status` both pipeline
  `ZSCORE queue {userID}` + `GET matchmaking_active_game:{userID}` in one
  Redis round-trip before touching anything else.
- Failover staleness accepted as bounded, not solved further: during the
  window after an owning instance dies and before either the entry expires
  or a new owner resumes renewal, a lookup may miss (limbo, same shape as
  this project's already-accepted crash-before-resolve gap,
  `DECISIONS_LOG_PHASE_2.md` ADR-031's Consequences) or briefly return a
  stale `instanceLabel`. A stale `instanceLabel` fails the connect attempt
  cleanly and the client falls back to `/resolve` — the exact recovery path
  `ConnectClaims`' own short-TTL design already relies on
  (`VerifyConnectToken`'s doc comment). Not a new failure mode.

---

## ADR-038: Voluntary Queue Cancellation

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

Closing ADR-036's queue-starvation timeout surfaced that nothing let a client
voluntarily leave the queue — only the timeout sweep removed anyone.

**Options considered:**

**Option A: No cancel endpoint; rely solely on the timeout (ADR-036).**
- Cons: rejected — forces every voluntary cancellation (a player who simply
  changes their mind) to wait out the full timeout window with no way to
  leave sooner, and provides no immediate confirmation to the client.

**Option B: `DELETE /matchmaking/queue`, same `MatchmakingClaims` token,
`ZREM queue {userID}` (CHOSEN).**
- Pros: immediate, client-initiated, symmetric with `POST /matchmaking/queue`.

**Decision:** Option B.

**Rationale:**

A plain synchronous HTTP response is sufficient here — no SSE push needed,
unlike the timeout case. The client making the `DELETE` call already knows
the outcome from the response itself; the SSE push mechanism (ADR-036)
exists specifically for server-initiated changes the client isn't actively
causing, which a voluntary cancel is not.

**Consequences:**
- New endpoint, minimal: `DELETE /matchmaking/queue`, auth via the existing
  `MatchmakingClaims` token (no new token type).
- No interaction with ADR-037's active-game marker — cancellation only ever
  applies to a queued-not-yet-matched player; a matched player has nothing
  to cancel through this path.

---

## ADR-039: Correcting ADR-034's UUID-Convention Rationale

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

ADR-034's Rationale cited `internal/store/game_store.go`'s `CreateGame` doc
comment ("game.ID (UUID v4)") as evidence that UUID v4 is this codebase's
general DB-primary-key convention, using that as part of its justification
for choosing v4 for the new `matchmaking_request_id` column. Independent
review, followed by direct re-verification against `internal/game/manager.go`
(the actual call site, not the store-layer doc comment describing an
expectation on the caller), found this citation was to a stale comment:
`Manager.CreateGame` generates `games.id` via `uuid.NewV7()` explicitly,
with its own comment stating "UUID v7: time-ordered, better B-tree index
locality than v4." `games.id` is v7, not v4 — `game_store.go`'s doc comment
is itself out of date relative to the code that actually sets the field it
describes.

**Correction:**

The stated justification in ADR-034 was wrong; the decision it supported was
not. `matchmaking_request_id` is a correlation/idempotency token, checked by
exact match (`ON CONFLICT (matchmaking_request_id) DO NOTHING`), never
range-scanned or used for ordering — the specific property v7 provides over
v4 (better B-tree locality for sequentially-inserted, sequentially-queried
rows) does not apply to it regardless of what convention `games.id` actually
follows. A plain random v4 remains the right, simpler choice for this column
on its own merits, with no dependency on `games.id`'s actual generation
scheme one way or the other.

**Decision:** ADR-034's Decision (`matchmaking_request_id` as UUID v4)
stands, unchanged. Its Rationale's supporting claim about matching an
existing codebase-wide convention is retracted — no such convention claim is
needed or should be relied on.

**Consequences:**
- ADR-034's original text is not edited in place, per this project's
  append-only discipline (same pattern ADR-031 used to correct ADR-030).
  This entry is the authoritative correction.
- `internal/store/game_store.go`'s `CreateGame` doc comment ("game.ID
  (UUID v4)") is a separate, pre-existing source-code inaccuracy, not
  introduced by this design pass — noted here since it's what caused the
  original miscitation, but its fix belongs in an implementation session,
  not this one.

---

## ADR-040: In-Process + Redis Pub/Sub for SSE Fan-Out — Considered and Rejected

**Date:** 2026-08-08
**Status:** ACCEPTED

**Context:**

An in-process (non-extracted) design using Redis pub/sub for cross-instance
SSE fan-out — chess-server remains a single deployable; whichever instance
performs a pairing `PUBLISH`es a match-found event to a channel every
instance `SUBSCRIBE`s to, and whichever instance happens to hold the
relevant player's SSE connection delivers it locally — was the mechanism
used by this project's prior, fully-discarded Phase 3 pre-planning attempt
(discarded 2026-08-06; that attempt's discard was for hallucinated,
unverified content found elsewhere in its execution, not because this
specific mechanism was itself shown to be flawed). It was not raised or
evaluated during the from-scratch design pass that produced ADR-032
(confirmed: it does not appear among that pass's Options A through D). It is
logged here, deliberately, so that if it resurfaces in a future session,
there is a documented, honest evaluation already on record rather than
requiring it to be re-derived from nothing each time, or mistaken for a
novel idea nobody has yet considered.

**Evaluated on its own technical merits — not dismissed by association with
the discarded attempt it originated in:**

This is not the same category of pub/sub design `DECISIONS_LOG_PHASE_2.md`
ADR-021 rejected. ADR-021 rejected pub/sub for maintaining two
independently-converging copies of ongoing, mutable game state — a
genuinely hard distributed-consensus problem. A one-time, fire-and-forget
match-notification event is a different use case entirely; pub/sub is
reasonably well-suited to exactly this kind of fan-out, and it would solve
the SSE N×N delivery problem ADR-032 raises — a subscribed instance holding
the relevant connection can receive and forward the event regardless of
which instance performed the pairing.

**Rejected anyway, for a reason specific to this option, not inherited from
ADR-021:**

Redis pub/sub has no delivery guarantee, no persistence, and no replay. A
subscriber that is momentarily disconnected from Redis, or not yet
subscribed at the moment of publish (a real startup-ordering race, not
hypothetical), loses the message permanently with nothing to retry against.
ADR-033's chosen design has no equivalent gap by construction: chess-server
retries the gRPC report itself with backoff, matchmaking-service dedups via
a Redis key, and `GET /matchmaking/status` (ADR-036) exists specifically as
a backstop for the case push delivery fails outright. An in-process pub/sub
design would need to independently reconstruct an equivalent backstop — at
minimum, still requiring some polling endpoint on chess-server — while also
introducing a third connection paradigm (SSE, alongside chess-server's
existing WebSocket and REST) into a service that currently has two, for no
offsetting benefit over keeping SSE in the service that was going to need
that backstop endpoint anyway.

**Decision:** Not adopted. ADR-032's Decision (separate service,
server-picks) stands, unaffected.

**Consequences:**
- No code or infrastructure change — this ADR exists purely as a documented
  record of consideration.
- Retroactively added to ADR-032's Options list as a fifth
  considered-and-rejected option, for completeness. ADR-032's original text
  is not edited in place, per this project's append-only discipline — this
  ADR is the authoritative record of the addition.

---

## ADR-041: First-Move Grace Period — Move-Triggered `ACTIVE→ABORTED` for a Game That Never Really Started

**Date:** 2026-08-10
**Status:** ACCEPTED

**Context:**

TD-P3-004's original framing covered only the *connection*-shaped half of
"a matched game with no real content" (opponent never connects at all).
Working through it surfaced a structurally different but related gap,
scoped by the user as part of the same session: a game that reaches
`ACTIVE` (both players' WebSockets connected — confirmed against
`session.go`'s `RegisterConnection`: activation requires
`s.playerWhite != nil && s.playerBlack != nil`, independent of any move
having been played) but where White never plays a first move, or plays it
and Black never responds, has **no termination mechanism of any kind**
today. Traced directly against `manager.go`: `onAbandonTimeout` only ever
fires from a `HandleDisconnect`-armed timer — if both players simply stay
connected and one side never moves, nothing arms anything, and the game
sits `ACTIVE` forever. Separately, even where a disconnect *does* occur
during this window, the existing mechanism produces a wrong result, not
just no result: `onAbandonTimeout`'s post-`WAITING` branching scores an
`ACTIVE` game's abandonment as `COMPLETED` (opponent wins) or `ABANDONED`
(draw) unconditionally — for a game with zero real moves played, both
outcomes are as misleading as the exact problem `DECISIONS_LOG_PHASE_2.md`
ADR-029 was written to fix for `WAITING`-status games. This is a
pre-existing latent gap in already-shipped Phase 1/2 code, not something
Phase 3 introduces — matchmaking only makes it far more likely to be hit in
practice (two strangers paired automatically are more likely to have one
side hesitate or drop before the first move than two players who
deliberately shared a link).

**Options considered:**

**Option A: Do nothing beyond existing mechanisms.** Rejected — as shown
above, the existing disconnect-triggered timer only fires on an actual
disconnect, and produces a misleading scored outcome even when it does
fire. It does not address the case (the core of scenario 1) where both
players remain connected and one simply never moves.

**Option B: Extend `onAbandonTimeout`'s existing disconnect-triggered timer
with a move-count check, still keyed off disconnect.** Rejected — the
triggering event is wrong. The actual failure mode this needs to catch
("time elapsed since `ACTIVE` with too few moves played") has nothing
structurally to do with a disconnect; gating discovery on a disconnect
event that may never happen (both players can remain technically connected
indefinitely without either one moving) leaves the core scenario uncaught.

**Option C: A new, independent, move-count-gated timer — armed at the
`WAITING→ACTIVE` transition (for White's opening move) and re-armed once,
at White's first move, for Black's reply — entirely decoupled from
connection state (CHOSEN).**

**Decision:** Option C, with the following load-bearing sub-decisions,
each resolved explicitly rather than left to accident:

1. **New state-machine edge: `ACTIVE→ABORTED`**, gated at fire-time to only
   apply when the actual move count is still below the timer's expected
   threshold (1 for White's window, 2 for Black's — reusing
   `GameStateSnapshot.Moves`'s length, already computed by
   `CurrentStateSnapshot`, no new query needed for the live-fire path).
   Reuses `ABORTED`'s existing meaning and wire/DB representation exactly
   (`outcome`/`outcome_reason` stay `NULL`, wire payload
   `outcome: "", reason: "ABORTED"`, same as the existing
   `WAITING→ABORTED` edge) — a game with zero or one moves played was never
   really contested, the same principle ADR-029 already established, now
   applied to an `ACTIVE`-status game rather than only a `WAITING`-status
   one. No new status value, no new wire message type.

2. **Suppression, not independence, versus the ordinary per-color
   disconnect-abandon timer.** Tracing the "what if a player disconnects
   during this window" question (item 5) concretely: if the ordinary 60s
   timer is left armed *in addition to* this one, an unlucky firing order
   (most likely at hydration, where `effectiveDisconnectedAt`'s fallback
   could compute a *shorter* remaining duration than a freshly-armed
   first-move timer if the disconnect happened well before the hydrate)
   lets the ordinary timer resolve the game as a scored `COMPLETED`/
   `ABANDONED` outcome *before* the first-move timer gets a chance to void
   it correctly — reintroducing the exact misleading-outcome problem this
   ADR exists to close, just via a race instead of unconditionally.
   Decision: while a game is `ACTIVE` with fewer than 2 moves played, the
   ordinary per-color disconnect-abandon timer is **not armed at all** —
   neither from a live `HandleDisconnect` call nor from hydration
   (`armAbandonTimersForGame`'s call sites). The first-move timer is the
   sole governing mechanism for this window, full stop. This directly
   answers items 6 and 7: the timer is purely move-triggered and
   indifferent to connection churn in between (item 6) — reconnecting does
   nothing to it because disconnecting did nothing to it either — and it
   fires on schedule even if both players disconnect, since nothing about
   its arming or firing consults connection state at all (item 7).

3. **A real gap found while closing item 5, not previously visible:**
   suppressing the ordinary timer *while* the first-move window is open is
   not sufficient on its own — nothing then re-arms it for a player who is
   *still* disconnected at the moment the window concludes (Black's reply
   is persisted, retiring the first-move mechanism entirely). Without an
   explicit bootstrap at that exact moment, a player who disconnected
   during the suppressed window and never reconnected would have no timer
   of any kind protecting the game from that point forward — a silent gap,
   not a loud one, since nothing throws an error, the game would simply
   never resolve. Fix: at the same point the first-move mechanism retires
   (move count reaching 2), for each color still disconnected at that
   instant, arm the ordinary abandon timer fresh (a normal, full
   `abandonTimeout`, with a normal disconnect-timestamp persist — treated
   identically to an ordinary `HandleDisconnect`-observed disconnect
   happening at that moment, not a special case downstream of this point).

4. **Persisted state for failover (item 10, item 4 scenario c):** unlike
   the opponent-never-connects mechanism (ADR-042), this one needs to
   survive failover with real precision, not a generous re-arm — the
   window itself is only 15–20s, an order of magnitude tighter than the
   60s disconnect grace period, so "just give it a fresh window at
   whatever point someone happens to hydrate" would systematically be far
   more generous than intended, for the same reason `DECISIONS_LOG_PHASE_2.md`
   ADR-031 rejected exactly that shortcut ("Option A") for the disconnect
   case. New nullable `games.activated_at TIMESTAMPTZ` column, set once,
   atomically with the same statement that persists the `WAITING→ACTIVE`
   status transition (same best-effort/non-fatal semantics already
   governing that write in `HandleConnect`'s `activated` branch — a
   `NULL` `activated_at` on a genuinely-`ACTIVE` game is a real, if narrow,
   possible outcome of that non-fatal write failing, not only a
   hypothetical pre-migration row). Black's window needs **no new column**
   — `moves.played_at` (confirmed present on `store.Move`, always written
   atomically with the move itself per the Move Processing Pipeline's
   persist-before-broadcast rule, so never ambiguously `NULL` for a move
   that exists at all) is already the correct anchor. At hydration, a new
   `NULL`-fallback helper mirroring `effectiveDisconnectedAt`'s exact
   shape and reasoning (trust the persisted value when present; assume the
   window starts now only when genuinely unknown) computes the correct
   remaining duration for whichever window is still open, or arms nothing
   if move count has already reached 2.

5. **Scope: universal, not matchmaking-only (item 8).** Nothing about the
   underlying defect — a disconnect (or simple non-move) during the
   pre-first-move window producing a misleading scored outcome — is
   matchmaking-specific; it is exactly as reachable via `CreateGame`/
   `JoinGame` today as it will be via `CreateMatchedGame`. This follows
   `DECISIONS_LOG_PHASE_3.md` ADR-037's precedent (extending
   check-before-enqueue to shared-link games "since nothing about it is
   inherently matchmaking-specific") on stronger grounds than that ADR
   had: this is not a consistency argument but a correctness one — leaving
   it shared-link-scoped means knowingly leaving a live, already-shippable
   bug unfixed in already-shipped Phase 1/2 code simply because it was
   noticed while designing something else. **Flagged explicitly as a
   material behavior change to shipped Phase 1/2 code, not just new Phase
   3 surface** — any existing Phase 1/2 test asserting a scored outcome
   for a disconnect that happens to occur before either player's first
   move will need to be re-examined against this new behavior (it should
   now expect `ABORTED`, not `COMPLETED`/`ABANDONED`). This is called out
   here for explicit confirmation rather than silently folded in, given
   its blast radius beyond Phase 3.

6. **Clock timing (item 13) — resolved as a non-issue, not a bug.** The
   chess clock starts at the same `WAITING→ACTIVE` instant the first-move
   window opens (`session.clock.Start(ColorWhite)`, confirmed in
   `manager.go`'s `HandleConnect`), so White's clock does tick during
   their own first-move window. Traced through deliberately rather than
   assumed away: this is not a special cost the grace period imposes — a
   chess clock ticking for whichever side is on move, including their very
   first move, is ordinary, universal chess-clock behavior, identical in
   kind to any other move taking 15–20 seconds of thought anywhere else in
   the game. The grace period does not add a deduction on top of the
   clock; it only adds a *ceiling* (void the game if the window is
   exceeded), which is orthogonal to how the clock already worked. Since a
   game the window actually times out on is voided (`outcome`/
   `outcome_reason` `NULL`), any clock time consumed before that point is
   moot regardless. No change to clock start/pause behavior.

**Rationale:**

The deciding factor across sub-decisions 1–3 is the same one this
project's own Constraint 15/16 discipline keeps surfacing: a mechanism that
looks complete on first pass (suppress the competing timer, done) is not
actually complete until the *reverse* transition (suppression lifting) is
also traced through explicitly — the gap in sub-decision 3 would have
shipped silently otherwise. Sub-decision 4 follows `DECISIONS_LOG_PHASE_2.md`
ADR-031's own established precedent directly (trust a real persisted
anchor over a generous guess, whenever a real anchor is available) rather
than re-deriving a weaker answer for a superficially similar problem.
Sub-decision 5 is the more consequential call in this ADR and is flagged
as such rather than buried in Consequences.

**Consequences:**
- Migration: `games.activated_at TIMESTAMPTZ`, nullable — new migration,
  not yet an on-disk file (mirrors `matchmaking_request_id`'s own
  ADR-034-era treatment: planned here, written at implementation time).
  `store.Game`/`GameStateSnapshot` gain the field where needed.
- `session.go` `validTransitions` gains `GameStatusActive: { GameStatusAborted: true, ... }` (additive to the existing `Completed`/`Abandoned` edges, not a replacement).
- New in-process timer, sharing `Manager.abandonTimers`' map and mutex with
  a distinct, collision-safe key shape (e.g. `gameID+":FIRSTMOVE"` — never
  equal to any real `store.Color` value) rather than a new map — the map's
  doc comment needs updating to describe the broadened key-space, not just
  `gameID+":"+color`, per `CODING_GUIDELINES.md` §3's "document what a
  mutex/its guarded state protects" spirit.
- `armAbandonTimersForGame`'s three existing call sites (`HandleConnect`'s
  registry-miss fallback, `ResolveGame`'s claim-and-hydrate branch,
  `restoreGame`) need a wrapping gate: for a `WAITING`-status game, existing
  behavior is unchanged; for an `ACTIVE`-status game with move count < 2,
  arm the new first-move timer instead of the ordinary per-color timers;
  for an `ACTIVE`-status game with move count ≥ 2, existing behavior is
  unchanged. `armAbandonTimersForGame` itself does not need to change
  internally — only whether/when its call sites invoke it.
- New integration point in the move-processing pipeline
  (`internal/game/move.go` — **not read this session; needs direct
  verification against its actual structure before implementation**, per
  this project's own "documentation is not source of truth" principle):
  after a confirmed move-number-1 persist, arm Black's first-move window;
  after a confirmed move-number-2 persist, retire the mechanism entirely
  and run sub-decision 3's disconnected-player bootstrap.
- Recommend factoring the shared "transition to `ABORTED`, persist
  (outcome nil), notify, finalize" block into one private helper reused by
  `onAbandonTimeout`'s existing `WAITING` branch and this ADR's new
  `onFirstMoveTimeout` — implementation-time refactor, not mandated here.
- No new wire message type; no new Redis state; no interaction with
  `DECISIONS_LOG_PHASE_3.md` ADR-037's `matchmaking_active_game` marker
  beyond what ADR-042 already covers (both new `ABORTED` triggers
  unregister via the existing `finalizeGame`, which is what stops marker
  renewal — see ADR-042's Consequences for the direct trace).
- `firstMoveTimeout = 20 * time.Second` — picked from the top of the
  stated 15–20s range: the cost of a false positive (voiding a real game
  because a player took 16s to think) is worse than the cost of a false
  negative (a genuinely-abandoned zero-move game taking a few extra
  seconds to resolve), the same asymmetric-cost reasoning
  `DECISIONS_LOG_PHASE_2.md` ADR-031 used for its own "assume disconnected"
  default.

---

## ADR-042: Matched-Opponent-Never-Connects — Arm-at-Creation-Time Timeout (Closes TD-P3-004)

**Date:** 2026-08-10
**Status:** ACCEPTED

**Context:**

TD-P3-004, restated precisely: a matched player who connects and *stays*
connected, while their assigned opponent never connects at all, has no
abandonment mechanism, because no `HandleDisconnect` call ever fires for
either color (the connected player never disconnects; the absent one never
connected to disconnect from) — confirmed again this session by direct
re-read of `HandleConnect`/`HandleDisconnect`/`onAbandonTimeout`, not
assumed from `PHASE_3.md`'s prior description of the trace.

**Re-verification of the already-resolved half, performed fresh rather
than cited (item 3):** for the case where the connected player *also*
eventually disconnects, `onAbandonTimeout`'s `WAITING`-branch check
(`snap.Status == store.GameStatusWaiting`) is the sole gate into the
`ABORTED` path, with no reference anywhere in that branch, or in
`RegisterConnection`'s activation check, to how many players were assigned
at creation versus how many actually connected. A `CreateMatchedGame` game
that reaches this state is structurally identical, at the code level, to a
`CreateGame` game whose second player never joined. Confirmed still
correct, unchanged by anything in ADR-041 above (ADR-041's new mechanism
only engages once a game has actually reached `ACTIVE`, which this case
never does).

**Verification of `DECISIONS_LOG_PHASE_2.md` ADR-031's fallback coverage
for the crash+failover variants (item 1, item 4):** traced concretely, not
assumed. Take the case where only White ever connected before the owning
instance crashes, and afterward White (or, symmetrically, Black — the
mechanism is color-agnostic) is the party who eventually calls `/resolve`
or reconnects. Hydration (`armAbandonTimersForGame`) reads both persisted
disconnect timestamps — both `NULL`, since White's "disconnect" was never
individually observed (the process died) and Black's never applied at all
(Black never connected in the first place) — and `effectiveDisconnectedAt`
resolves both `NULL`s to "assume disconnected as of now" for this
`WAITING`-status game, arming a **fresh 60s defensive timer for both
colors**. Whichever side is reconnecting cancels their own timer via the
ordinary `HandleConnect` path; the still-absent side's defensive timer
runs its course and fires `onAbandonTimeout`, which (status still
`WAITING`, since the reconnecting side alone cannot activate the game)
correctly resolves to `ABORTED`. **Confirmed, not corrected**: the claim
holds exactly as suspected — this crash+failover variant is already closed
by ADR-031's existing side effect, *provided at least one party attempts
to connect/resolve after the crash*. What remains genuinely uncovered,
and can only be uncovered by construction, is the conjunction the task
named precisely: both players never connect at all, **and** nobody ever
attempts to connect or resolve after a crash either — a game nothing ever
touches again, crash or no crash. This is the actual target of this ADR.

**Options considered:**

**Option A: Per-process `time.AfterFunc`, armed synchronously at
`CreateMatchedGame` time, same shape as the existing `abandonTimers`
mechanism and its existing per-process-only limitation (CHOSEN).**
- Pros: closes the steady-state (no crash) gap completely and
  unconditionally — it does not depend on any future event (a disconnect,
  a resolve call, a heartbeat tick) to exist at all, since it is armed the
  moment the game is created, before anything else can happen.
- Cons: does not survive the owning instance dying before it fires.

**Option B: A periodic check folded into the existing heartbeat tick
(`internal/game/heartbeat.go`), reusing the "one loop appended-to" pattern
`DECISIONS_LOG_PHASE_3.md` ADR-037 used for its own marker renewal.**
- Pros: superficially more "robust" — doesn't depend on a single
  in-process timer surviving.
- Cons: rejected. It provides **zero additional failover coverage** over
  Option A for the one case that actually matters: the heartbeat tick only
  runs on an instance that has the game locally registered and is itself
  alive. If the owning instance dies, its heartbeat loop dies with it —
  identical to Option A's timer dying with it. The only way Option B would
  close the crash+nobody-resolves residual gap is as a Postgres-level
  sweep fully decoupled from any single instance's local registry — a
  genuinely different, heavier component (closer to
  `DECISIONS_LOG_PHASE_2.md` ADR-031's own "Option D," a periodic sweep
  across all locally-registered sessions, which that ADR explicitly
  declined to build ahead of demonstrated need). Building that here, for a
  gap that is already an accepted, tracked, bounded characteristic of this
  system (TD-P2-006), repeats a build-ahead-of-need mistake this project
  has repeatedly and deliberately avoided (ADR-014, ADR-016, ADR-023's
  TD-P2-001/003 deferrals, ADR-031's own Option D deferral).

**Decision:** Option A.

**Rationale:**

Once the crash+resolve variant is confirmed already closed by existing
machinery (above), the only coverage gap Option B could offer over Option A
is precisely the crash+nobody-resolves case — and Option B does not
actually close that case either, for the reason given. With no coverage
difference between the options for any case that isn't already handled,
precision and simplicity favor Option A outright: an exact per-game timer
with no need to compute elapsed-since-creation on every tick, no new
persisted anchor, no change to the heartbeat loop's existing
responsibilities.

**Duration (item 9):** a new, distinctly-named constant —
`matchedOpponentConnectTimeout = 60 * time.Second` — deliberately the same
*value* as `abandonTimeout` but a separate symbol, not a shared one.
Reusing the value follows this project's demonstrated preference for
reusing an existing, already-load-tested number over inventing a new one
without evidence it needs to differ (`DECISIONS_LOG_PHASE_3.md` ADR-037
reused `OwnershipTTL`/`OwnershipRenewInterval` on the same reasoning;
ADR-036 mirrored `ConnectClaimsTTL`'s pattern for `MatchmakingClaimsTTL`).
Giving it its own symbol rather than referencing `abandonTimeout` directly
keeps the two concepts (reconnection grace vs. first-ever-connection
grace) independently tunable later without one change silently affecting
the other's meaning.

**Scope (contrast with ADR-041): matched games only, not universal.** This
is the opposite scope call from ADR-041 above, made deliberately, not
inconsistently: a shared-link game's creator waiting for *anyone* to click
the link has no specific-named-opponent promise attached to it — indefinite
waiting is ordinary, acceptable UX (share the link again, wait as long as
you like). A matched player has been given a specific, system-asserted
promise ("we found you an opponent") that a shared-link creator never
receives; that promise is what carries an implicit SLA a shared-link game
does not have. The two mechanisms in this session solve genuinely
different problems (a promise not kept, versus a game with no real
content) for genuinely different populations, and are scoped accordingly.

**Consequences:**
- New timer, sharing `Manager.abandonTimers`' map/mutex with a
  collision-safe key (e.g. `gameID+":OPPONENT_NEVER_CONNECTED"`).
- Armed synchronously inside `Manager.CreateMatchedGame` (not yet
  implemented — this is a requirement on that method's eventual
  implementation, not a change to existing code), in the same call as the
  atomic insert / `GameRegistry.Register` / `ClaimOwnership` sequence
  `DECISIONS_LOG_PHASE_3.md` ADR-032's Consequences already describe for
  that method.
- Cancelled at the moment `RegisterConnection` returns `activated=true`
  (`HandleConnect`'s existing `if activated` branch, alongside the
  existing `session.clock.Start` call) — the opponent showed up, the
  promise was kept, this concern is moot from that point forward.
- At fire: reuses the exact same `WAITING→ABORTED` transition, persistence
  shape, and wire payload as `onAbandonTimeout`'s existing `WAITING`
  branch — no new state-machine edge needed here (unlike ADR-041). As
  in ADR-041, recommend factoring that shared block into one helper
  reused by both the existing `onAbandonTimeout` `WAITING` branch and this
  new trigger.
- `finalizeGame` should additionally cancel this new timer on every
  terminal transition, for the same resource-hygiene reasons it already
  cancels the two per-color abandon timers — not required for correctness
  (a stale fire against an already-`ABORTED`/-terminal game is a
  guaranteed no-op via the existing `Transition` state-machine guard,
  same "harmless dead data" pattern `DECISIONS_LOG_PHASE_2.md` ADR-030
  already accepted for stale disconnect timestamps) but worth doing since
  timers hold live runtime resources, unlike a stale DB column.
- **Confirms `DECISIONS_LOG_PHASE_3.md` ADR-037's marker self-heals
  correctly for this new termination path (item 12), traced directly
  rather than assumed:** `heartbeat.go`'s `heartbeatTick` renews ownership
  (and, per ADR-037's design, will renew the `matchmaking_active_game`
  marker) only for `m.registry.AllActive()`'s current contents; `finalizeGame`
  unregisters on every terminal transition, this one included. Once
  unregistered, the next tick simply stops including this game, and the
  marker ages out within one `OwnershipTTL` window with no explicit
  delete required — identical self-healing shape to every other terminal
  path, not a special case.
- **New interaction with ADR-037, not previously surfaced, noted rather
  than silently accepted (item 14):** because `CreateMatchedGame` writes
  the `matchmaking_active_game` marker for *both* assigned players eagerly
  at match time (ADR-037's Consequences: "created eagerly... at
  `CreateMatchedGame` success (entries for both)"), a player who never
  connects at all will still show as having an active game — and be
  blocked from queueing again — for up to `matchedOpponentConnectTimeout`
  (60s), even though they never actually connected to it. This is a
  bounded, self-healing, arguably-correct consequence (a player who was
  validly matched into a still-nominally-open game arguably *should* be
  blocked from a second concurrent match until the first resolves one way
  or the other) rather than a bug, but is recorded explicitly so it is not
  mistaken for one later.

---
