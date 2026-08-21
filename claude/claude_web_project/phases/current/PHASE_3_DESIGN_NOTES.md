# Phase 3 — Matchmaking Design Notes

**Status:** Category decision confirmed (topology + pairing model). Mechanism-level
design (match-report RPC shape, re-enqueue semantics, SSE endpoint/auth) is
explicitly open — see §6. Do not treat this document as a finished spec; it
is a working record of the category decision and the flow it implies,
written so the reasoning survives to the next session. A formal ADR should
be written once §6 is resolved, not before — writing it now would mean
revising a "decided" record mid-flight.

**Supersedes:** An earlier in-process-matchmaking recommendation (goroutine
inside chess-server, package-isolated). That recommendation under-weighted
the cross-instance SSE-delivery problem — once traced through concretely,
it argues for extraction instead. See §3.

---

## 1. Category Decision

Two independent choices were made. They are evaluated on different axes and
easy to conflate — stating both explicitly on purpose:

1. **Topology: separate matchmaking-service, own container.** Not a
   goroutine inside chess-server.
2. **Pairing decision: server-picks.** chess-server instances themselves
   decide who ends up owning a given match, by contending directly for
   pairs against the shared Redis queue. matchmaking-service does **not**
   assign, round-robin, or broadcast-and-race to pick a winning instance.

---

## 2. What "server-picks" means here (and what it rules out)

Ruled out, explicitly, so this doesn't get re-litigated by accident later:

- **Round-robin / least-loaded assignment** by matchmaking-service — would
  require it to track live chess-server instance membership and load,
  neither of which exists anywhere in this codebase today. The LB's static
  instance map is the closest analog, and it's deliberately dumb/mechanical
  by design (Phase 2, ADR-022) — this would be new surface area, not reuse.
- **Broadcast-and-race** — matchmaking-service fanning a candidate pair out
  to every chess-server instance and taking the first ACK as the winner.
  Technically workable — chess-server's existing `ClaimOwnership` CAS
  (`expectedPriorOwner=""` = first-claim-wins) already gives race semantics
  for free — but pays an N-instance RPC fan-out on *every single match* for
  zero correctness benefit over the option below, which gets the same
  single-winner guarantee from the queue primitive itself.
- **Streaming service-assigns** — chess-server instances holding a
  persistent bidirectional gRPC stream to matchmaking-service, which pushes
  assignments over it. A real, structurally different RPC pattern
  (streaming vs. unary), named as a live alternative during the category
  discussion. **Not chosen** — it adds persistent-connection lifecycle
  management (reconnect/backoff on stream drop) to avoid a broadcast cost
  that server-picks already avoids without needing any stream at all.

**What we're actually doing:** every chess-server instance runs its own
pairing loop against the *same* Redis sorted-set queue. `ZPOPMIN queue 2` is
atomic — if two instances race to pop, Redis guarantees exactly one of them
gets any given pair. No coordination between chess-server instances is
needed for the pop itself; the atomicity of the primitive **is** the
mechanism. matchmaking-service is never in the path of deciding who plays
whom, or which instance handles it — that falls out of whichever instance's
`ZPOPMIN` call happens to land first, exactly like `ClaimOwnership` already
works today.

---

## 3. Why extraction, not in-process

The deciding factor was SSE delivery, not the matchmaking logic itself.

A player's match-notification SSE connection is sticky to whichever process
accepted it (LB has no reason to route it anywhere consistent). If
matchmaking logic runs inside chess-server, any of N instances could pair a
match while either player's SSE stream sits on a *different* one of N
instances — an N×N delivery problem. chess-server is built around
single-owner, no-fan-out state (Phase 2, ADR-021); it has no good place to
absorb a problem shaped like that.

Extracting matchmaking-service collapses this: chess-server never needs to
know or care which of matchmaking-service's own replicas is holding a given
player's SSE stream. It makes one RPC call to the service after creating the
game; the service resolves its own internal delivery. chess-server-to-
chess-server communication is eliminated entirely — the N×N mesh problem
doesn't get built in the first place, rather than being solved carefully.

---

## 4. Component responsibilities

| Component | Owns | Does NOT do |
|---|---|---|
| **matchmaking-service** | `POST /matchmaking/queue` → enqueues into the shared Redis sorted-set queue (score = enqueue timestamp, for fairness). Holds each waiting player's SSE connection, keyed by userID. Receives the match-report RPC from whichever chess-server instance wins a pair, and pushes the resulting `MATCH_FOUND` event down the right SSE connection(s). Handles failure reports by notifying the affected client(s) to retry. | Never picks or assigns players to a chess-server instance. Never touches `GameStore`, `GameRegistry`, or Redis ownership keys. Never decides which instance handles a pair — that decision is already made by the time it hears about it. |
| **chess-server (each instance)** | Runs its own periodic pairing loop against the shared queue (`ZPOPMIN queue 2`). On a successful pop, runs `CreateMatchedGame` locally — atomic two-player DB insert, local `GameRegistry` registration, `ClaimOwnership` — mirroring `CreateGame`'s existing eager pattern. Reports the outcome (success with gameID + connect info, or failure) to matchmaking-service over gRPC. | Never talks to another chess-server instance directly. Never holds or knows about any SSE connection. |

---

## 5. End-to-end flow

```
1. Client A                 -> POST /matchmaking/queue   -> matchmaking-service
2. matchmaking-service      -> ZADD queue <ts> <userA>    -> Redis
3. Client A                 -> GET  /matchmaking/stream   -> matchmaking-service  (opens SSE, held open)
   (Client B does the same independently — may land on a different LB hop
   in front of matchmaking-service; doesn't matter, both are still owned by
   matchmaking-service's own replica set, not by chess-server)

4. chess-server instance-2's pairing loop (running independently on every
   chess-server instance, on its own timer):
       ZPOPMIN queue 2  -> Redis
       -> wins the pop for (userA, userB)   [atomic; only one instance can win]

5. instance-2: Manager.CreateMatchedGame(ctx, userA, userB)
       -> atomic INSERT, both players in one statement  -> Postgres
       -> registry.Register(session)                     -> local, in-memory
       -> ClaimOwnership(gameID, "instance-2")            -> Redis

6. instance-2 -> gRPC MatchCreated(gameID, userA, tokenA, userB, tokenB,
                                    instanceID="instance-2")
             -> matchmaking-service

7. matchmaking-service:
       -> push SSE "MATCH_FOUND" to userA's stream:
              { gameID, token: tokenA, connect: "/connect/instance-2" }
       -> push SSE "MATCH_FOUND" to userB's stream:
              { gameID, token: tokenB, connect: "/connect/instance-2" }

8. Client A dials /connect/instance-2 directly with tokenA
   Client B dials /connect/instance-2 directly with tokenB
   (same resolve-then-connect token shape as the existing shared-link flow —
   no new token mechanism; matchmaking-service is just delivering what the
   client would otherwise have had to call /resolve itself to get)
```

**Failure branch** (step 5 fails after step 4 already popped the pair off
the queue — this is the case that needs care, since the players are already
gone from the queue with no game to show for it):

```
5a. instance-2: atomic INSERT fails (retries, if any, exhausted)
6a. instance-2 -> gRPC MatchFailed(userA, userB, originalScoreA, originalScoreB)
             -> matchmaking-service
7a. matchmaking-service: either re-enqueues both players at their original
    score (preserving queue position/fairness — they should not lose their
    place for a server-side failure that wasn't their fault) or pushes an
    SSE "MATCHMAKING_FAILED" event telling the client to retry manually.
    Which of these is the right default, and whether it differs by failure
    type, is open — see §6.
```

---

## 6. Match-Report RPC Contract (decided)

Unary gRPC, two methods, not one method with a success/failure oneof — the
two outcomes share almost no fields and have completely different
downstream handling on matchmaking-service's side, so a oneof would mostly
produce a message where half the fields are meaningless depending on branch.

`instance_label` (not `instance_id`) is used deliberately — it matches the
field name already used end-to-end in the real codebase
(`auth.ConnectClaims.InstanceLabel`, `Manager.ResolveGame`'s
`instanceLabel` return value, nginx.conf's `$instance_label`/ADR-022's
"instanceLabel is opaque" language), confirmed by direct read of
`resolve.go` and `nginx.conf` — not introducing a second name for the same
concept.

```protobuf
syntax = "proto3";

package matchmaking.v1;

option go_package = "github.com/vedant-2701/matchmaking-service/proto/matchmakingv1;matchmakingv1";

// MatchReportService is called by chess-server instances after a pairing
// attempt resolves. matchmaking-service has no involvement in *which*
// instance handles a pair (server-picks, §2) — this is purely a report of
// an outcome that has already happened.
service MatchReportService {
  rpc ReportMatchCreated(MatchCreatedRequest) returns (MatchCreatedResponse);
  rpc ReportMatchmakingFailed(MatchmakingFailedRequest) returns (MatchmakingFailedResponse);
}

message MatchCreatedRequest {
  string game_id        = 1;  // idempotency key for this RPC — see below
  string white_user_id  = 2;
  string black_user_id  = 3;
  string white_token    = 4;  // signed PlayerClaims, same shape /resolve issues today
  string black_token    = 5;
  string instance_label = 6;  // matches ConnectClaims.InstanceLabel — see §9 for what
                               // matchmaking-service is/isn't allowed to assume about it
}

message MatchCreatedResponse {
  bool acknowledged = 1;
}

message MatchmakingFailedRequest {
  string white_user_id = 1;
  string black_user_id = 2;
  MatchmakingFailureReason reason = 3;
}

// Enum, not a free-form string — CODING_GUIDELINES.md's forbidden-patterns
// table already bans hardcoded strings for state in favor of typed
// constants; no reason to relax that at a service boundary.
enum MatchmakingFailureReason {
  MATCHMAKING_FAILURE_REASON_UNSPECIFIED       = 0;
  MATCHMAKING_FAILURE_REASON_RETRIES_EXHAUSTED = 1;  // only case that exists today;
                                                       // left as an enum so a second
                                                       // cause later doesn't force a
                                                       // field-shape change
}

message MatchmakingFailedResponse {
  bool acknowledged = 1;
}
```

**Scope of `ReportMatchmakingFailed`:** only fires for a *confirmed,
terminal* failure of `CreateMatchedGame`'s atomic DB insert, after retries
are exhausted — mirroring `CreateGame`'s existing failure semantics
(ADR-028): a failed `ClaimOwnership` is separately already established as
non-fatal and best-effort, and `CreateMatchedGame` should not deviate from
that for consistency. It must **not** fire for every transient/retryable
insert failure — see §8 for what happens to those instead.

**Reliability:**
- chess-server retries the RPC itself, bounded, with backoff. The game
  already exists in Postgres by the time either RPC fires — a failed
  *report* is never allowed to roll back or retry game creation. Same
  shape as `CreateGame`'s ownership claim and `HandleDisconnect`'s
  timestamp persist: log and move on if retries exhaust.
- `game_id` is a natural dedup key for `MatchCreated` — matchmaking-service
  keeps a short-TTL Redis key (`reported:<gameID>`) checked before pushing
  SSE, covering a chess-server retry after a lost ACK.
  `MatchmakingFailed` has no natural key (no game exists yet); accepted
  at-least-once without dedup — a duplicate "still searching" hint is
  low-severity compared to inventing an attempt-ID just to dedupe a rare
  terminal-failure path.
- If the RPC never succeeds at all: falls back to the poll endpoint named
  in §10 — client polls, sees the game exists via the ordinary
  player-state lookup, same backstop pattern as SSE-vs-polling elsewhere
  in this design.

## 7. Cross-cutting decisions (finalized)

- **Trust boundary:** shared secret, sent as gRPC call metadata and
  checked by a server-side interceptor on `MatchReportService`. Chosen over
  network policy specifically because this is Docker Compose, not
  Kubernetes — Compose has no declarative per-service network-policy
  primitive equivalent to a K8s `NetworkPolicy`; any container on the same
  Compose network can reach any other by default, so "network policy" isn't
  actually an available mechanism here, only an aspirational one. A shared
  secret is simple, explicit, testable, and is itself real practice writing
  a gRPC auth interceptor. Documented as a deliberate interim choice —
  swap for mTLS post-Phase-3, not solved now.
- **Proto/codegen location:** monorepo. chess-server and matchmaking-service
  share one `proto/` directory in this same repo.
- **matchmaking-service language:** Go, consistent with the rest of the
  project.

## 8. Re-enqueue ownership — recommendation recorded, decision left open

Two options were on the table: chess-server re-enqueues directly (one Redis
`ZADD`, no RPC), or matchmaking-service re-enqueues (requires an RPC report
first). The concern raised: does chess-server doing it directly introduce a
race? Traced through concretely:

**Clean failure case (a definite, unambiguous DB error):** no race found.
`ZPOPMIN` already atomically removed both players from the queue before
`CreateMatchedGame` runs — while they're out, no other instance can touch
them. `ZADD` re-adding them is naturally idempotent (sorted-set members are
unique; re-adding the same member just updates its score), so even an
accidental double re-enqueue by chess-server is safe by construction.

**The actual hazard, found by tracing further:** an *ambiguous* failure —
chess-server's insert attempt times out or the ACK is lost, and it can't
tell whether the `INSERT` actually committed before declaring failure and
re-enqueuing. If it re-enqueues in that state and the insert had in fact
succeeded, the two players end up both matched to a live game *and* back in
the queue, eligible to be paired again — a double-booking. This risk is
**orthogonal to who performs the re-enqueue** — matchmaking-service
re-enqueuing on chess-server's say-so inherits the exact same risk, since
it can only act on whatever ambiguous signal chess-server reports.

**Recommended fix, independent of the ownership question:** a
`matchmaking_request_id` column on `games` (`UUID`, `UNIQUE`), generated
once by chess-server per pairing attempt (right after the `ZPOPMIN` that
won it), reused across all retries of that same attempt, insert issued with
`ON CONFLICT (matchmaking_request_id) DO NOTHING` (or an existence check
before declaring failure). This makes retries safe regardless of ACK loss —
failure is only ever declared once genuinely confirmed. Use UUID v4 —
**correction, found during later independent review
(`DECISIONS_LOG_PHASE_3.md` ADR-039): the text originally here claimed this
matched `game_store.go`'s `CreateGame` doc comment (`game.ID (UUID v4)`) as
evidence of a codebase-wide v4 convention. That comment is stale — the
actual generating call site, `internal/game/manager.go`'s `CreateGame`, uses
`uuid.NewV7()` explicitly (`games.id` is v7, not v4). The v4 choice for
`matchmaking_request_id` itself is unaffected and still correct on its own
merits: it's a correlation/idempotency token checked by exact match, never
range-scanned or ordered, so v7's specific benefit (B-tree locality for
sequential range access) never applied to it regardless of what `games.id`
actually uses.**

**Given that fix, the recommendation is chess-server owns re-enqueue
directly** (matches the framing already used for `MatchCreated`/
`MatchmakingFailed` in §6 — the RPC only fires for the terminal case).
Once the ambiguous-failure hazard is closed at the insert layer, routing
re-enqueue through matchmaking-service adds an RPC on every transient
failure with no remaining correctness benefit to justify it — the increased
RPC volume concern raised is valid and unopposed by any offsetting gain.

**DECIDED, confirmed** — chess-server owns re-enqueue directly, with the
`matchmaking_request_id` idempotency column as a prerequisite piece of the
same change.

## 9. Connect-info field — kept as-is for now, real gap flagged instead

The concern raised (a server reachable by a different IP/domain shouldn't
break this) pointed at something real, confirmed by reading `nginx.conf`
and `docker-compose.yml` directly:

- Today, `/connect/{instanceLabel}` is a **relative path**, safe only
  because the client is already talking to nginx's single public origin
  (port 8080) for both the REST API and the WS upgrade — nginx's `map`
  mechanically dereferences the label to the right upstream container.
- `nginx.conf` currently has **no location block for matchmaking-service at
  all**. Whether matchmaking-service will share that same public origin
  (a new `location /matchmaking { ... }` added to the existing server
  block) or be exposed separately (its own port/origin) is not yet decided
  — and this, not the RPC field shape, is the actual open question.
- If matchmaking-service shares the same origin: the existing relative-path
  convention keeps working unmodified for SSE-delivered connect info, same
  as it does for `/resolve` today — `instance_label` alone (already in the
  proto, §6) is sufficient, and no `connect_url`/`connect_path` field is
  needed.
- If matchmaking-service gets a separate origin: a bare relative path
  delivered over matchmaking-service's own SSE stream is ambiguous to the
  client (relative to which host?) and this **would** break — at that
  point `instance_label` alone stops being sufficient and a fully-qualified
  URL field becomes necessary.

**DECIDED, confirmed** — matchmaking-service shares the existing nginx edge
proxy's single public origin. `instance_label` alone (already in the §6
proto) is sufficient; no `connect_url`/`connect_path` field is added. New
consequence, tracked in §11: the added `location /matchmaking` block needs
`proxy_buffering off` (SSE-specific — nginx's default response buffering
silently breaks real-time delivery) and the same extended `proxy_read_timeout`
the existing WS block already uses.

## 11. SSE Endpoint Design (proposed)

### Token

`PlayerClaims` doesn't fit — it requires a `GameID` that doesn't exist yet
at queue-time. Client identity going into matchmaking is the same bootstrap
as `CreateGame`: a self-asserted, client-generated `userID` (per
`store/models.go`: *"Identity is anonymous: userID is generated
client-side"*), sufficient to enqueue but not sufficient to safely hold an
SSE stream — anyone who knows/guesses a `userID` could otherwise open a
stream and intercept someone else's eventual `PlayerClaims` token. New claims
type, same signing secret as everything else (per `token.go`'s stated rule —
one secret in this codebase, distinguished by shape):

```go
// MatchmakingClaims scopes an SSE connection to a specific queued userID.
// Minted by POST /matchmaking/queue on successful enqueue; verified once at
// SSE connection open, same as ConnectClaims is verified once at WS upgrade
// and never re-checked for the life of the connection.
type MatchmakingClaims struct {
    UserID string `json:"user_id"`
    jwt.RegisteredClaims
}

const MatchmakingClaimsTTL = 60 * time.Second // mirrors ConnectClaimsTTL's reasoning exactly
```

### Endpoints

- **`POST /matchmaking/queue`** — enqueues (`ZADD queue NX <ts> <userID>`)
  and mints a fresh `MatchmakingClaims`. Uses Redis's `NX` flag deliberately:
  makes the call idempotent, so a client that calls it again (refresh, retry,
  reconnect after a long drop) never resets their queue position, but still
  gets a usable fresh token back either way — no separate re-mint endpoint
  needed. **Known gap, not solved here:** if this is called after the player
  was already matched (client was offline when `MatchCreated` fired), `NX`
  alone won't catch it — they're no longer in the queue to no-op against, so
  a naive call would silently re-enqueue someone who already has a game. The
  handler needs a check-before-enqueue against already-matched state first.
  Tracked in §12.
- **`GET /matchmaking/stream?token=...`** — SSE, verifies `MatchmakingClaims`
  once at connection open (mirrors WS upgrade handling), then holds the
  connection, pushing `MATCH_FOUND`/`MATCHMAKING_FAILED` events as they
  arrive from the gRPC report handler's internal dispatch.
- **`GET /matchmaking/status?token=...`** — same token, single JSON response
  (`waiting` / `matched` + connect info / `failed` + reason). Serves two
  triggers that both reduce to "the push channel can't be relied on right
  now": a browser/proxy that can't do `text/event-stream` falls back to
  polling this on a timer, and it's the same backstop already referenced in
  §6/§9 for a lost RPC or dropped SSE push. One endpoint, not two fallback
  mechanisms.

### nginx consequence (actionable now that §9 is decided)

New `location /matchmaking` block needs `proxy_buffering off` — nginx's
default response buffering silently breaks SSE's real-time delivery — plus
the same extended `proxy_read_timeout` (3600s) the existing WS block already
carries, since a queued player may legitimately hold the stream open for
minutes. Server-side SSE keepalive comments (`: heartbeat\n\n` on an
interval) cover anything in between that doesn't respect that timeout. No
new library — stdlib `http.Flusher` + chi, same as the rest of the API.

### Scaling caveat (flagged for symmetry with §3)

matchmaking-service's SSE connections live in an in-memory map
(`userID → flusher`) — fine at one replica (M=1, already accepted in §3). If
matchmaking-service is ever scaled beyond one replica, it inherits the exact
structural problem that got matchmaking logic extracted out of chess-server
in the first place (§3): a routing/ownership layer would be needed for its
own SSE connections. Extraction solved chess-server's N×N problem by moving
it, not eliminating it — the same shape reappears if matchmaking-service
itself needs to scale out. Not a concern at M=1; on record so it isn't a
surprise later.

## 12. Pairing-loop timer (decided)

Fixed interval, configurable via env var, read once at startup — no
separate config file. Nothing else in this project reads one
(`.env.example`, `env-check.sh`, `docker-compose.yml`'s `INSTANCE_ID` are
all plain env vars); a config file would be new tooling introduced for no
reason. Suggested name: `MATCHMAKING_PAIRING_INTERVAL_MS`, sane default TBD
at implementation time. Queue-starvation behavior (one player waiting, no
match, indefinitely) is not a timer-interval question and remains open —
see §14.

## 13. Check-before-enqueue mechanism (decided)

### The problem, precisely

`POST /matchmaking/queue`'s `ZADD ... NX` (§11) makes *repeat* calls safe
for a player who is still waiting — but does nothing for two related gaps:
a player who is already **queued** calling it again gets no signal that
they're already waiting, and a player who already has an **active game**
(matched via matchmaking, *or* via the existing shared-link
`CreateGame`/`JoinGame` flow) calling it would be silently enqueued for a
second, concurrent game. Confirmed as in-scope for the shared-link case
specifically, not just matchmaking-originated games: a player mid-game via
a shared link should not be able to also queue for a random match.

These are two independent facts with two different sources of truth, and
are answered independently rather than merged into one status key —
merging them would require matchmaking-service to become a *second writer*
of "queued" state that already lives, uncontested, in the queue itself:

### Fact 1: "Is this user already queued?"

Answered by the existing queue ZSET directly — `ZSCORE queue {userID}`.
No new key. The ZSET is already the source of truth for queue membership;
a copy of that fact would only be a second place for it to disagree with
itself. `ZPOPMIN` popping a member makes `ZSCORE` return nil automatically
— no explicit cleanup needed.

### Fact 2: "Does this user already have an active game (any game)?"

Answered by a new Redis key, **written and renewed by chess-server only,
never by matchmaking-service**:

**Key:** `matchmaking_active_game:{userID}` — deliberately not `game:*`,
which already names the *ownership* key (`game:{gameID}`, gameID → owning
instance) in `directory.go`. Same prefix for a userID-keyed, opposite-
direction mapping would be ambiguous in `redis-cli KEYS 'game:*'` later.

**Value (JSON):**
```json
{ "gameID": "...", "connectToken": "...", "instanceLabel": "...", "wsPath": "..." }
```
**Correction, found while closing §14:** this was originally written as
`{game_id, instance_label, token}` (snake_case), copied by habit from the
§6 gRPC proto's own convention. That's wrong for this key — confirmed
against `resolve_test.go`'s `resolveResponseData` assertions
(`connectToken`, `instanceLabel`, `wsPath`), the REST API already has an
established camelCase convention, and this cache is read only by
matchmaking-service to build client-facing JSON, so it should just store
that exact shape directly — no translation layer needed for this piece,
unlike the gRPC-failure-reason mapping in §14. `connectToken` is that
specific player's existing signed `PlayerClaims` — not a new credential.
Confirmed via `token.go`: `PlayerClaims` carries a 24h expiry
(`SignPlayerToken`'s doc comment: "24h recommended"), so it can be minted
once and republished unchanged on every renewal below with no staleness
risk on any timescale relevant here. `wsPath` is precomputed
(`/connect/{instanceLabel}`) the same way `/resolve` already precomputes
it, rather than making every reader reconstruct it.

**Scope:** every locally-owned game with status `WAITING_FOR_PLAYER` or
`ACTIVE` — the same set `idx_games_status`'s partial index already covers
— regardless of whether it originated via `CreateGame`/`JoinGame` or
`CreateMatchedGame`. One entry per currently-assigned player (so a
`WAITING_FOR_PLAYER` game with only `player_white_id` set has one entry,
not two).

**Who writes, and when:**
- **Created eagerly, synchronously**, at each of: `CreateGame` success
  (entry for `player_white_id`, reusing the `PlayerClaims` `CreateGame`
  already mints today), `JoinGame` success (entry for `player_black_id`,
  same reuse), `CreateMatchedGame` success (entries for both). This closes
  what would otherwise be an ~`OwnershipRenewInterval`-wide gap (see next
  point) between game creation and the first heartbeat tick, during which
  a just-created solo `WAITING_FOR_PLAYER` game's creator could still slip
  a matchmaking-queue call through.
- **Renewed every `OwnershipRenewInterval` (10s)**, by the *same* per-tick
  heartbeat loop that already batch-renews ownership keys via
  `RenewOwnershipBatch` for this instance's locally-active games — one more
  write appended to an iteration that already runs, not a new goroutine.
- **TTL: `OwnershipTTL` (30s)** — same constant, same margin reasoning
  (`DECISIONS_LOG_PHASE_2.md` ADR-023) already governing ownership keys,
  not a new number invented for this.
- **Deleted by nobody.** Once a game leaves `{WAITING_FOR_PLAYER, ACTIVE}`,
  or its owning instance dies, the heartbeat loop simply stops including it
  in the next tick's iteration — the entry ages out within one `OwnershipTTL`
  window on its own. Same self-healing property as the ownership keys
  themselves; no reconciliation worker, because there is no explicit delete
  step that could fail to fire.

**Who reads:** matchmaking-service only, read-only. `POST
/matchmaking/queue` and `GET /matchmaking/status` both do one pipelined
Redis round-trip — `ZSCORE queue {userID}` + `GET
matchmaking_active_game:{userID}` together — before touching anything else.
Queued (Fact 1 hit) → tell the client to keep waiting, no re-enqueue.
Active game found (Fact 2 hit) → return `{gameID, connectToken,
instanceLabel, wsPath}` directly (§15's `matched` shape) — client-side
handling doesn't need a special case for "found via queue-check" vs. "just
matched." Neither → proceed with `ZADD NX`.

**Failover staleness, accepted as bounded:** if the owning instance dies,
this entry expires within `OwnershipTTL` same as the ownership record does
— a lookup in that window may `MISS` (limbo, same shape as this project's
already-accepted crash-before-resolve gap) or briefly hand back a stale
`instance_label`. A stale `instance_label` fails the connect attempt
cleanly and the client falls back to `/resolve` — the exact recovery path
`ConnectClaims`' own short-TTL design already relies on
(`VerifyConnectToken`'s doc comment: a rejected stale connect forces
"re-call resolve", not a hang). Not a new failure mode.

## 14. Queue-starvation behavior (decided)

Bounded wait, not indefinite. matchmaking-service runs its own periodic
sweep — a separate ticker from chess-server's pairing-loop ticker (§12);
different service, different concern, but reusing "one loop, not a new
worker per fact" the same way §13's heartbeat-appended writes did. The
sweep scans the queue ZSET for members older than
`MATCHMAKING_QUEUE_TIMEOUT_SECONDS` (configurable, same env-var convention
as §12), `ZREM`s them, and pushes `MATCHMAKING_FAILED` with
`reason: QUEUE_TIMEOUT` to their SSE stream if still connected.

This surfaces a real distinction worth being precise about: a queue
timeout **never touches gRPC** — chess-server never attempted a pairing for
a player who timed out, so it has nothing to report. `MatchmakingFailureReason`
(the proto enum, §6) correctly stays scoped to what chess-server can
actually experience (`RETRIES_EXHAUSTED` only, unchanged). matchmaking-service
needs its own client-facing reason type instead — a superset: `RETRIES_EXHAUSTED`
(translated from the gRPC enum when that's the trigger) plus `QUEUE_TIMEOUT`
(generated natively, never sent over gRPC at all). This is exactly the case
§6's own comment anticipated when the proto enum was left open rather than a
bool ("left as an enum so a second cause later doesn't force a field-shape
change") — confirms that call was right, not just a hedge.

**Gap found while closing this out, not previously specified:** nothing
lets a client *voluntarily* leave the queue — only a timeout removed
anyone. Adding `DELETE /matchmaking/queue` (same `MatchmakingClaims` token,
`ZREM queue {userID}`). A plain synchronous HTTP response is sufficient
here, no SSE push needed — the client making the call already knows the
outcome from the response itself; the push mechanism exists specifically
for server-initiated changes the client isn't actively causing, which is
what the timeout case actually is and this isn't.

## 15. SSE / status JSON payload shapes (decided)

Field names mirror `/resolve`'s existing response exactly (`connectToken`,
`instanceLabel`, `wsPath`, confirmed against `resolveResponseData` in
`resolve_test.go`) so a client's SSE/status handling and its `/resolve`
handling can share one parsing shape with no special-casing.

SSE uses its native `event:` line as the discriminator (idiomatic —
lets a client `addEventListener` per type), rather than a `type` field
inside `data:` — SSE already has a mechanism built for exactly this,
no reason to duplicate it. `GET /matchmaking/status` has no equivalent
native discriminator, so it uses an explicit `status` field instead —
different mechanisms for genuinely different transport shapes, not an
inconsistency between them.

```
event: MATCH_FOUND
data: {"gameID":"...","connectToken":"...","instanceLabel":"...","wsPath":"/connect/..."}

event: MATCHMAKING_FAILED
data: {"reason":"RETRIES_EXHAUSTED"}   // or "QUEUE_TIMEOUT", per §14
```
SSE `data:` payloads above are correct as shown, unwrapped — confirmed
deliberately during reconciliation: they are stream events, not HTTP
request/response pairs (one HTTP status covers the whole stream's
lifetime, not one per event), so `CODING_GUIDELINES.md` §7's envelope does
not apply to them. `GET /matchmaking/status` below is an ordinary HTTP
response and does need it — **correction, found during reconciliation
against `internal/api/response.go`'s actual `writeData`: this was
originally shown unwrapped here, which was wrong; every plain JSON HTTP
response in this project is wrapped in `{"data": ...}`, no exceptions
other than `GET /health`:**

```json
{"data": {"status":"waiting"}}
{"data": {"status":"matched","gameID":"...","connectToken":"...","instanceLabel":"...","wsPath":"..."}}
{"data": {"status":"failed","reason":"RETRIES_EXHAUSTED"}}
```

## 16. Nothing left open

Every item raised across this design pass is now decided: topology (§1),
pairing model (§1–§2), extraction rationale (§3), component responsibilities
(§4), end-to-end flow (§5), RPC contract (§6), trust boundary/proto
location/language (§7), re-enqueue ownership (§8), matchmaking-service
origin (§9), SSE tokens and endpoints (§11), pairing-loop timer (§12),
check-before-enqueue (§13), queue-starvation and cancel (§14), and payload
shapes (§15). The formal ADR write-up this document deferred was completed
as ADR-032 through ADR-040. §17 below is a second, later pass (2026-08-10),
closing TD-P3-004 (`DECISIONS_LOG_PHASE_3.md` ADR-041/ADR-042) — the one
piece this document's original 16 sections deliberately left for a
dedicated session.

---

## 17. Matched-Game Connection-Grace Semantics (TD-P3-004, closed 2026-08-10)

Full decisions and rationale: `DECISIONS_LOG_PHASE_3.md` ADR-041 (first-move
grace period) and ADR-042 (matched-opponent-never-connects). This section
carries the scenario-by-scenario trace and mechanism-level detail those two
ADRs summarize, matching this document's usual role relative to the ADR log.

### 17.1 Two independent mechanisms, not one

Working through the brief's scenarios split cleanly into two triggers that
do not belong in a single decision, confirming the brief's own suspicion:

| Mechanism | Trigger | Status transition | Scope | Failover strategy |
|---|---|---|---|---|
| First-move grace period (ADR-041) | Move-count-gated timer, armed at `WAITING→ACTIVE` and re-armed once at White's first move | New `ACTIVE→ABORTED` edge | Universal — shared-link and matched games | New persisted `games.activated_at` + existing `moves.played_at`; real remaining-duration computation at hydration, mirroring `effectiveDisconnectedAt` |
| Matched-opponent-never-connects (ADR-042) | Per-process timer, armed synchronously at `CreateMatchedGame` | Existing `WAITING→ABORTED` edge (no new edge) | Matched games only | None needed directly — covered as a side effect of `DECISIONS_LOG_PHASE_2.md` ADR-031's existing fallback for the crash+resolve case; crash+never-resolve remains an accepted TD-P2-006-shaped residual gap |

### 17.2 Scenario-by-scenario resolution index

1. **First-move windows (both halves).** ADR-041, Decision + sub-decision 1.
2. **Matched opponent never connects at all.** ADR-042 in full — this is TD-P3-004's original core case.
3. **Mid-game disconnect after ≥1 move each.** Re-verified fresh, not cited: `onAbandonTimeout` branches purely on `snap.Status`, no origin check anywhere in the path. Unchanged, still correct. See ADR-042's Context.
4. **Crash+failover, all sub-cases.** (a) one-connected-crash-then-resolve: covered by `DECISIONS_LOG_PHASE_2.md` ADR-031's existing `effectiveDisconnectedAt` fallback as a side effect — traced concretely in ADR-042's Context, not assumed. (b) mid-game-both-connected-crash: identical in shape to the already-verified Phase 2 case (ADR-030/031's original motivating scenario); `CreateMatchedGame` mirrors `CreateGame`'s pattern closely enough that nothing distinguishes them at the code level. (c) crash during the first-move window itself: requires ADR-041's new `activated_at`-based hydration re-arm specifically — the generic disconnect-timestamp fallback is not precise enough for a 15–20s window (see 17.4). Residual, accepted, unsolved: both-never-connect AND nobody-ever-resolves — same shape as TD-P2-006, new instance of the same accepted class, not a new problem.
5. **First-move timer vs. disconnect-abandon timer interaction.** Suppression, not independence — see 17.3 below for the full trace, including a real gap (the "who re-arms the disconnect timer once suppression lifts" question) found only by tracing the *reverse* transition explicitly.
6. **Reconnect churn during the first-move window.** No effect — purely move-count-gated by design (ADR-041 sub-decision 2).
7. **Both disconnect before White's first move.** Timer still fires on schedule, `ABORTED`, independent of connection state at fire time — direct consequence of sub-decision 2's design, not a separate mechanism.
8. **Scope: shared-link too?** Yes, universal — ADR-041 sub-decision 5. Explicitly flagged there as a material behavior change to already-shipped Phase 1/2 code, not just new Phase 3 surface; existing abandonment regression tests for a disconnect before any move now need to expect `ABORTED`, not a scored outcome.
9. **Duration parity for scenario 2.** Reuse the *value* (60s) under a distinct symbol (`matchedOpponentConnectTimeout`), not a shared constant — ADR-042's Duration section.
10. **New persisted state.** `games.activated_at TIMESTAMPTZ`, nullable, new migration (not yet on disk, same treatment `matchmaking_request_id` got under ADR-034). Black's window needs no new column — `moves.played_at` (confirmed present on `store.Move`, always atomic with the move itself) is already sufficient. No new Redis keys for either mechanism — both are in-process only, following `Manager.abandonTimers`' existing shape, for the reasons in ADR-042's Options.
11. **Arm-at-creation mechanism shape.** Per-process `time.AfterFunc`, not a heartbeat-tick check — ADR-042's Options/Rationale: a heartbeat-tick alternative was traced through concretely and found to provide zero additional failover coverage over the simpler option, since a dead instance's heartbeat loop is exactly as dead as its in-process timer.
12. **ADR-037 marker self-heals for both new triggers?** Confirmed directly, not assumed — both call `finalizeGame`, which unregisters from `GameRegistry`; `heartbeat.go`'s `heartbeatTick` only renews for `registry.AllActive()`'s current contents, so the marker simply stops being renewed and ages out within one `OwnershipTTL` window, identical to every other terminal path. See ADR-042's Consequences for the exact trace, and a new, previously-unsurfaced interaction this closure revealed (a never-connected player's marker still blocks a second queue attempt for up to `matchedOpponentConnectTimeout`, a bounded, arguably-correct side effect of ADR-037's own eager-write-for-both-assigned-players design, not a bug).
13. **Clock timing vs. first-move window.** Traced and resolved as a non-issue — ADR-041 sub-decision 6. The clock ticking during a player's own first move is ordinary chess-clock behavior, not a cost the grace period specifically imposes; a timed-out game is voided anyway, so any clock time consumed before that point is moot.
14. **Anything else?** Two things surfaced that weren't named in the original scenario list: the disconnect-timer re-arm gap in item 5 (17.3), and the ADR-037 marker interaction in item 12. Both incorporated into the ADRs directly rather than left as follow-up notes.

### 17.3 The suppression/re-arm trace in full (item 5)

The naive version of "suppress the ordinary disconnect timer while the
first-move timer is active" is exactly half a design: it correctly prevents
the race where the ordinary timer might resolve the game (with a misleading
scored outcome) before the first-move timer gets the chance to void it —
but it says nothing about what happens to a player who disconnects during
the suppressed window and is *still* disconnected once the window
concludes (Black's reply lands, retiring the first-move mechanism
entirely). Suppression that never lifts is a silent, permanent gap:
nothing throws, nothing logs an error, the game simply never resolves for
that still-disconnected player from that point on, because the ordinary
timer was never armed for them in the first place and nothing is watching
for the moment it should have been.

Fix, folded into ADR-041 sub-decision 3: at the exact moment the
first-move mechanism retires (move count reaching 2), check both colors'
connection state; for whichever color is currently disconnected, arm the
ordinary abandon timer fresh from that instant — full `abandonTimeout`,
with a normal disconnect-timestamp persist, exactly as if `HandleDisconnect`
had observed a live disconnect at that moment. This makes the transition
back to ordinary semantics a real, positive action taken at a known point,
not an assumption that ordinary machinery will somehow "just resume" on
its own — it will not, because it was never armed to begin with.

### 17.4 Why ADR-041 needs a real persisted anchor and ADR-042 does not

Both mechanisms face a version of the same question — how does a
hydrating instance compute the right remaining duration for a timer it
doesn't remember arming? — and land on different answers, deliberately:

- **ADR-041 (first-move):** needs a real anchor (`activated_at` /
  `moves.played_at`), because the window is short (15–20s) relative to how
  late a hydrate might occur. A generous "just restart the window at
  whatever point someone happens to hydrate" answer would systematically
  run long, for the identical reason `DECISIONS_LOG_PHASE_2.md` ADR-031
  rejected that exact shortcut ("Option A") for the disconnect-timestamp
  case: discarding real, available information about elapsed time is
  strictly worse than using it when it's known.
- **ADR-042 (opponent-never-connects):** needs no new anchor at all,
  because the failover case it would otherwise need one for is *already*
  closed by ADR-031's existing disconnect-timestamp fallback, as a side
  effect neither mechanism has to engineer for directly (see 17.2 item 4).
  Building persistence for this mechanism specifically would duplicate
  coverage that already exists for a different reason.

The two mechanisms are not treated inconsistently by having different
answers here — they face genuinely different failover exposure, traced
through independently rather than defaulted to a single "house style" for
both.

### 17.5 Implementation-level notes (non-binding, for whoever picks up Step 3)

- Both new timers can share `Manager.abandonTimers`' existing map and
  mutex, using key suffixes that can never collide with a real
  `store.Color` value (e.g. `gameID+":FIRSTMOVE"`,
  `gameID+":OPPONENT_NEVER_CONNECTED"`) rather than two new maps.
- Recommend factoring the shared "transition to `ABORTED`, persist with
  nil outcome, publish `GAME_OVER` with `reason: "ABORTED"`, finalize"
  block — currently inline in `onAbandonTimeout`'s `WAITING` branch — into
  one private helper, reused by that branch, ADR-041's new
  `onFirstMoveTimeout`, and ADR-042's new opponent-never-connected
  handler. Three call sites producing byte-identical DB/wire output is a
  DRY violation worth closing at implementation time, not mandated by
  either ADR as written.
- `internal/game/move.go` was not read this session (not in the file list
  the brief specified) — the exact integration points for arming Black's
  first-move window after move #1 and retiring the mechanism after move #2
  need direct verification against that file's actual pipeline structure
  before implementation, per this project's own "documentation is not
  source of truth" discipline. Do not assume the shape described in
  ADR-041's Consequences is exact code — it describes the required
  integration point, not verified call syntax.
- `games.activated_at`'s migration should set the column atomically in the
  same statement as the `WAITING→ACTIVE` status update (a new or extended
  `GameStore` method), not as a separate call — minimizes, though does not
  eliminate, the window for the two to partially diverge under the
  existing best-effort/non-fatal persistence semantics already governing
  that write.

---

## 18. Matched-Game Connect Flow — Skip `/resolve` on First Connect (IMPLEMENTED, 2026-08-17)

**Status: IMPLEMENTED and verified (`go build`/`vet`/`test -race`/
`test -tags integration -race`/`gofmt` all clean, 2026-08-17). Formal ADR
written as `DECISIONS_LOG_PHASE_3.md` ADR-044, per this document's own
rule (§16: "a formal ADR should be written once resolved, not before") —
this section is retained as the full trace/rationale record, same relation
to its ADR as §17 has to ADR-041/042.**

### 18.1 The bug that surfaced this

Running `e2e-phase3.sh scenario1` against a live cluster: `WSHandler`
rejected the WebSocket dial from `MATCH_FOUND`'s `connectToken` with 401,
logging `token instanceLabel does not match URL, tokenInstanceLabel=""`.
Decoding the JWT confirmed why: `{"game_id":...,"user_id":...,"color":
"WHITE","exp":...}` — a `PlayerClaims` payload, no `instance_label` claim
at all. Traced to §13's own decision, confirmed accurate: `connectToken` in
the active-game marker (and everything downstream of it — the 409
response, and, it turns out, `MATCH_FOUND` too, since `ReportMatchCreated`
relays the exact same `white_token`/`black_token` gRPC fields into both)
is deliberately the player's long-lived `PlayerClaims`, not a dialable
`ConnectClaims` — §13's own text: *"confirmed safe to mint once and
republish unchanged on every renewal with no staleness risk."*

That reasoning is correct **for the marker's actual read pattern**: a 409
check or a heartbeat-renewed key might be read an arbitrary, unbounded time
after it was last written, so a short-lived `ConnectClaims` minted at write
time would frequently already be dead by read time — `PlayerClaims` +
force a fresh `/resolve` call at read time is the right call there.

**But `MATCH_FOUND` doesn't have that staleness problem.**
`CreateMatchedGame` runs on one specific instance, claims ownership on
itself synchronously, right there — and the gRPC report to
matchmaking-service happens essentially instantly afterward. There is no
meaningful time for staleness to accumulate between minting and delivery.
The current design reuses the *marker's* token (built for a different
read pattern) for `MATCH_FOUND` purely because both happen to be minted in
the same `CreateMatchedGame` call — not because it's the right credential
for that moment. §13 never actually traced `MATCH_FOUND`'s own read
pattern separately from the marker's; it should have.

### 18.2 What changes, and what doesn't

**Confirmed by the person, explicitly:** skip `/resolve` on the initial
"just matched" connection; keep `/resolve` required for any *later*
reconnect. This is not a shortcut around `/resolve` — it's recognizing
that `MATCH_FOUND` delivery and a later reconnect attempt are genuinely
different moments with genuinely different staleness exposure, the same
distinction §13 already draws between the marker (needs `/resolve`) and
nothing (nothing in this codebase currently skips it) — this proposal adds
the first case that does, for a reasoned, specific reason, not a general
relaxation.

**A consequence traced through that wasn't in the person's original ask,
worth confirming before implementing:** if `MATCH_FOUND` (and the
equivalent `GET /matchmaking/status` "matched" response, which reads the
same `matchmaking_result:{userID}` record `ReportMatchCreated` writes) only
ever carried the fresh `ConnectClaims` and nothing else, a client that
receives it late — offline when the SSE push fired, only polls `/status`
minutes later — would get back an *already-expired* `ConnectClaims`
(exactly the marker's own staleness problem, just relocated one level up).
The fix has to include a fallback credential in the same response, not
just the fast path: **`MATCH_FOUND` and the "matched" status payload both
carry the fresh `connectToken` (dial immediately) AND the `playerToken`
(24h, unaffected by any of this) side by side.** A client that gets an
expired/rejected `connectToken` falls back to `/resolve` using
`playerToken` — identical recovery shape to the marker's own
"stale `instanceLabel` fails cleanly, client falls back to `/resolve`"
property (§13, Failover staleness paragraph), just applied one layer up.
This is why `/resolve` isn't being removed from the system anywhere — it
remains the correct, necessary fallback for exactly the case it already
covers; this proposal only skips it for the one case where skipping it is
provably safe (zero elapsed time between mint and delivery).

**A second consequence, purely a naming problem, but a real one:** once
`MATCH_FOUND`'s `connectToken` field becomes a genuinely different *type*
of credential than the 409 response's `connectToken` field (dialable vs.
not), they can no longer share the exact same field name across both
response shapes — that is precisely the "same JSON shape, different actual
meaning" trap that caused 18.1's bug in the first place, just moved to a
different pair of endpoints instead of fixed. The 409/marker's field is
renamed `playerToken` throughout (it always was one; the current name was
simply wrong, not a new decision) — propagating into `directory.go`'s
`ActiveGameMarker`, `mmsvc/queue.go`'s mirrored `activeGameMarker`, and
`mmsvc/response.go`'s `existingGame`. `matchFoundData` stops being a type
alias of `existingGame` (`internal/mmsvc/hub.go`'s `type matchFoundData =
existingGame`) — the two shapes have genuinely diverged now, and forcing
them to stay identical would just reintroduce the same trap under a
different name.

### 18.3 Proposed new flow

```
1. Client A/B  -> POST /matchmaking/queue   -> matchmaking-service  (unchanged)
2. Client A/B  -> GET  /matchmaking/stream  -> matchmaking-service  (unchanged, SSE held open)

3. instance-2's pairing loop wins the pair, runs CreateMatchedGame:
       -> atomic INSERT                                    -> Postgres
       -> registry.Register(session), ClaimOwnership        -> local + Redis   (unchanged)
       -> mints whitePlayerToken/blackPlayerToken  (PlayerClaims, 24h)         (unchanged —
          same tokens already used for the active-game marker, ADR-037)
       -> ALSO mints whiteConnectToken/blackConnectToken    (ConnectClaims,
          m.connectClaimsTTL, instanceLabel="instance-2")   <-- NEW

4. instance-2 -> gRPC MatchCreated(gameID, userA, userB,
                    whitePlayerToken, blackPlayerToken,      <-- renamed from white_token/black_token
                    whiteConnectToken, blackConnectToken,    <-- NEW proto fields
                    instanceLabel="instance-2")
             -> matchmaking-service

5. matchmaking-service:
       -> SetResult(userA, {status:"matched", gameID, connectToken:whiteConnectToken,
                             playerToken:whitePlayerToken, instanceLabel, wsPath})
       -> Hub.Notify(userA, MATCH_FOUND, <same shape>)
       -> (mirror for userB)

6. Client A/B: dial wsPath directly with connectToken — NO /resolve call.
   If that dial is rejected (connectToken expired/stale — late SSE delivery,
   long offline gap before a /status poll): fall back to
   GET /games/{gameID}/resolve with Authorization: Bearer playerToken,
   exactly the existing reconnect path, unchanged.
```

**Unrelated, unaffected by any of this:** the active-game marker itself
(written directly by `CreateGame`/`JoinGame`/`CreateMatchedGame`, read by
matchmaking-service's 409 check) keeps using `playerToken` exactly as §13
already decided — that read pattern's staleness exposure is real and
unchanged by this proposal.

### 18.4 Concrete touch points (for review, not yet applied)

| File | Change |
|---|---|
| `proto/matchmakingv1/matchmaking.proto` | `MatchCreatedRequest`: rename `white_token`/`black_token` → `white_player_token`/`black_player_token`; add `white_connect_token`/`black_connect_token`. Requires `make proto` regen. |
| `internal/auth/token.go` | `DefaultConnectClaimsTTL`: 10s → **30s** (person's explicit request — "10s is too short, network blip or reopen, 20-30s won't cause issues"; picking 30s as the round value). Already env-configurable since Step 5 (`CONNECT_CLAIMS_TTL_SECONDS`) — only the *default* changes. `TestConnectClaims_TTLIsShort`'s `> time.Minute` threshold still holds (30s < 60s), no test change needed. |
| `internal/game/manager.go` (`CreateMatchedGame`) | Mint `whiteConnectToken`/`blackConnectToken` (ConnectClaims, `m.connectClaimsTTL`, `InstanceLabel: m.instanceID`) alongside the existing PlayerClaims mint — reuses the exact minting call `resolve.go` already makes, no new logic, just a second call site. Return shape grows from 2 tokens to 4 (or a small struct — open to either). |
| `internal/matchmaking/pairing.go`, `reporter.go` | Thread the two new tokens through into `MatchCreatedRequest`. |
| `internal/game/directory.go` (`ActiveGameMarker`) | JSON field `connectToken` → `playerToken` (rename only — the stored value was always a PlayerClaims). |
| `internal/mmsvc/queue.go` (`activeGameMarker`, `matchmakingResult`) | `activeGameMarker`: same rename. `matchmakingResult`: keep `ConnectToken` (now genuinely a ConnectClaims), add `PlayerToken`. |
| `internal/mmsvc/response.go` (`existingGame`) | `ConnectToken` → `PlayerToken` (json `playerToken`). |
| `internal/mmsvc/hub.go` (`matchFoundData`) | Stop aliasing `existingGame` — becomes its own struct: `GameID`, `ConnectToken`, `PlayerToken`, `InstanceLabel`, `WSPath`. |
| `internal/mmsvc/reportserver.go` (`ReportMatchCreated`) | Populate both `ConnectToken` and `PlayerToken` in `SetResult` and `Hub.Notify`, from the two new gRPC fields. |
| `internal/mmsvc/handler.go` (`statusResponseData`) | Add `PlayerToken` field. |
| `internal/auth/token.go` | `DefaultMatchmakingClaimsTTL`: 10s → **60s** (§18.5, resolved) — sized to safely outlive the longest legitimate queue wait, not tuned independently. |
| `cmd/matchmaking-service/main.go` (`loadConfig`) | New startup validation: `MatchmakingClaimsTTL >= QueueTimeout + 15s`. Fails fast (same panic-on-bad-config treatment every other required-value check in this file already gets) if the two are configured inconsistently — e.g. `MATCHMAKING_QUEUE_TIMEOUT_SECONDS` raised without `MATCHMAKING_CLAIMS_TTL_SECONDS` following it up. | 
| `e2e-phase3.sh`, `phase3_step7_e2e_walkthrough.md` | Scenario 1's `/resolve` detour (added to work around 18.1's bug) reverts to a direct dial using `connectToken`. Worth *adding*, not removing, a demonstration of the `playerToken` + `/resolve` fallback path — e.g. disconnect after matching, reconnect via `/resolve`, confirm it still works — since that path remains real and needs its own coverage. |

### 18.5 `MatchmakingClaimsTTL` — RESOLVED

Same bug shape as `ConnectClaimsTTL`'s 10s default (§18.4): a player
sitting in queue past 10s who then polls `GET /matchmaking/status` or calls
`DELETE /matchmaking/queue` would find their own token already expired,
for a completely ordinary wait — not an edge case.

**Decision:** tie `MatchmakingClaimsTTL`'s default to safely exceed
`MATCHMAKING_QUEUE_TIMEOUT_SECONDS`, and validate the relationship at
startup rather than hope the two stay in sync across independent env vars.
A player's `MatchmakingClaims` token only ever needs to outlive the longest
they could legitimately still be queued — that's bounded: queue timeout
(default 30s) plus one sweep tick's worst-case delay (`sweep.go`'s fixed
5s interval) plus round-trip margin. Once a player is no longer queued
(matched, cancelled, or swept), an expired token no longer matters —
there's nothing left to check or cancel.

- `DefaultMatchmakingClaimsTTL`: 10s → **60s** (30s queue timeout + 5s
  sweep interval + comfortable slack — a round, generous number, not
  tightly optimized, since there is no cost to it being generous).
- `cmd/matchmaking-service/main.go`'s `loadConfig`: after resolving both
  `QueueTimeout` and `MatchmakingClaimsTTL`, require
  `MatchmakingClaimsTTL >= QueueTimeout + 15*time.Second` and fail fast
  (panic, same as every other required-config check in this file) if
  violated — catches exactly the case where `MATCHMAKING_QUEUE_TIMEOUT_SECONDS`
  gets raised in some future deployment without `MATCHMAKING_CLAIMS_TTL_SECONDS`
  following it up.

**Explicitly not built:** a `Status`-mints-a-fresh-token-on-every-call
refresh pattern (mirroring `Queue`'s idempotent re-mint on repeat calls).
Considered and rejected — the TTL-vs-queue-timeout invariant above already
fully closes the problem; adding a second mechanism on top would be
building config/behavior surface ahead of demonstrated need, the same
instinct against speculative machinery this project already applies
elsewhere (ADR-014, ADR-016).

