# Phase 3 — Matchmaking

**Status: ⬜ Not Started (implementation). Pre-planning: ✅ Complete.**
**Prerequisite: Phase 2 all acceptance criteria met**

> **Note on this document's status (added 2026-08-08):** This document has
> been reconciled against `DECISIONS_LOG_PHASE_3.md` ADR-032 through ADR-038,
> the from-scratch pre-planning pass that ran after the original pre-restart
> Phase 3 design (would-have-been ADR-032–036, discarded 2026-08-06 for
> hallucinated content) was thrown out in full. The topology and pairing
> questions this document originally posed as open ("Architecture Decision
> Required Before Phase 3") are now decided — see that section below for the
> resolution. Full reasoning trail: `DECISIONS_LOG_PHASE_3.md`,
> `phases/current/PHASE_3_DESIGN_NOTES.md`. ADR-032–038 were independently
> reviewed (fresh session, per this project's standing review discipline) —
> ADR-032, 033, 035, 037, 038 accepted as written; ADR-034 and ADR-036
> accepted with corrections (stale UUID-convention justification in ADR-034;
> `MatchmakingClaimsTTL`/`ConnectClaimsTTL` to be made env-configurable,
> default 10s, in ADR-036 — see that ADR's addendum). Implementation has not
> started; this document describes the decided design, not built state.

---

## Objective

Build a matchmaking system where players can enter a queue and be automatically paired into a game. The queue must be safe across multiple server instances — two servers must never match the same player twice.

**The single most important learning outcome of Phase 3:**
Distributed race conditions are real, silent, and dangerous. Atomic operations are the correct solution. Locks are the wrong solution at this scale.

---

## The Problem This Phase Solves

Phase 1 and 2 require players to share a link. That is not a real chess experience. Phase 3 adds: player enters the queue, waits, gets paired with another player, and a game starts automatically.

The naive implementation works fine on a single server. It breaks silently under multiple instances.

**The race condition:**

```
Server 1 reads queue: [PlayerA, PlayerB]  →  decides to match PlayerA + PlayerB
Server 2 reads queue: [PlayerA, PlayerB]  →  also decides to match PlayerA + PlayerB

Result: PlayerA is in two games simultaneously. PlayerB is in two games.
Both games have wrong state. Both players are confused.
```

This is not a hypothetical. It happens every time two workers race on the same queue.

---

## Architecture Decision (Resolved): Separate Service, Server-Picks

**This section originally posed the topology question as open ("Architecture
Decision Required Before Phase 3") and, inconsistently, the Scope section
below listed "in-process goroutine" as already-decided fact. Both were wrong
— see the reconciliation note at the top of this document. The actual
decision, made in the from-scratch pre-planning pass:**

- **Topology: `matchmaking-service` is a separate deployable** (own
  container, own `docker-compose.yml` entry), not a goroutine inside
  chess-server. Deciding factor: a player's match-notification connection is
  sticky to whichever process accepts it, and pairing can happen on any of N
  chess-server instances — in-process, this is an unsolved N×N delivery
  problem chess-server's single-owner/no-fan-out design (`DECISIONS_LOG_PHASE_2.md`
  ADR-021) has no clean place to absorb. Extraction collapses this to at most
  M×M inside matchmaking-service's own (currently M=1) replica set.
- **Pairing decision: server-picks, not service-assigns.** Each chess-server
  instance runs its own pairing loop (`ZPOPMIN queue 2`) against the shared
  Redis queue. `matchmaking-service` never decides who plays whom or which
  instance handles a pair — `ZPOPMIN`'s atomicity is the entire mechanism,
  the same primitive `ClaimOwnership` already uses for first-claim-wins
  semantics elsewhere in this codebase.
- **An in-process (non-extracted) alternative using Redis pub/sub for
  cross-instance SSE fan-out** — the mechanism the discarded prior Phase 3
  attempt used — **has been evaluated on its own technical merits and
  rejected.** It was not part of the from-scratch design pass that produced
  ADR-032 itself; it is documented separately, deliberately, as
  `DECISIONS_LOG_PHASE_3.md` ADR-040, specifically so it has an honest,
  substantive rejection on record rather than resurfacing as either a
  seemingly-novel idea or unexamined leftover contamination in some future
  session. See ADR-040 for the actual reasoning — it is not the same class
  of pub/sub design ADR-021 rejected for game state, and is rejected here
  for a different, specific reason (no delivery guarantee).

Full detail, options considered, and rationale: `DECISIONS_LOG_PHASE_3.md`
ADR-032 through ADR-038, `phases/current/PHASE_3_DESIGN_NOTES.md`.

---

## Scope

### In Scope

- Single queue: 10+0 time control only (expanding time controls is Phase 4)
- Redis sorted set (`matchmaking:queue:10+0`) as the queue data structure,
  owned by `matchmaking-service` for enqueue (`ZADD ... NX`) and by every
  chess-server instance for atomic dequeue (`ZPOPMIN`)
- `matchmaking-service`: new separate deployable — queue intake, SSE
  notification delivery, gRPC match-report receiver (ADR-032, ADR-033)
- chess-server: per-instance pairing loop + `Manager.CreateMatchedGame`
  (new `internal/matchmaking` package) — atomic two-player DB insert with
  `matchmaking_request_id` idempotency (ADR-034), local `GameRegistry`
  registration, `ClaimOwnership`, mirroring `CreateGame`'s existing eager
  pattern
- Players enter queue via `POST /matchmaking/queue` (matchmaking-service,
  behind the shared nginx edge proxy — ADR-035), exit via
  `DELETE /matchmaking/queue` (ADR-038) or automatic queue-timeout sweep
  (ADR-036 §14) or WebSocket/SSE disconnect
- Matched players notified via SSE (`GET /matchmaking/stream`), not a
  WebSocket connection to chess-server — see ADR-036 for why `PlayerClaims`
  doesn't fit this role and a new `MatchmakingClaims` type is used instead
- Check-before-enqueue via two independent Redis facts: `ZSCORE queue
  {userID}` (already queued) and a heartbeat-renewed
  `matchmaking_active_game:{userID}` lease key, written only by chess-server
  (ADR-037) — covers both matchmaking-originated and shared-link-originated
  active games
- ELO is not available yet — queue is FIFO by wait time

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| ELO-based matching | Phase 4: ELO doesn't exist yet |
| Multiple time controls in queue | Complexity without learning benefit at this stage |
| Geographic matching | Not a learning objective |
| Re-queue after no match found | Handled by the queue-timeout sweep + client-initiated `DELETE` + re-`POST` (ADR-036 §14, ADR-038), not a distinct mechanism |
| mTLS between chess-server and matchmaking-service | Shared-secret gRPC interceptor is the deliberate interim trust boundary (ADR-033) — Docker Compose has no `NetworkPolicy` equivalent. Revisit post-Phase-3. |

---

## Key Technical Challenges

### Challenge 1: ZPOPMIN vs Read-Then-Delete

**Wrong approach:**
```
Server 1: ZRANGE queue 0 1  →  gets [PlayerA, PlayerB]
Server 2: ZRANGE queue 0 1  →  gets [PlayerA, PlayerB]
Server 1: ZREM queue PlayerA PlayerB
Server 2: ZREM queue PlayerA PlayerB  ←  race condition: both matched the same pair
```

**Correct approach:**
```
ZPOPMIN queue 2  ←  atomic: removes AND returns two lowest-score members in one operation
```

`ZPOPMIN` is atomic. Two servers cannot pop the same pair. One will get the pair. The other will get an empty result or a different pair.

**What this teaches:** Atomic operations eliminate entire classes of race conditions. This is why Redis sorted sets are used for queues — not because they are the only option, but because their operations are atomic and the data structure fits the use case.

### Challenge 2: Odd Queue Numbers

What happens when 3 players are in the queue and the matchmaker runs?

```
Queue: [PlayerA, PlayerB, PlayerC]
ZPOPMIN queue 2  →  matches PlayerA + PlayerB
PlayerC remains in queue, waiting for a fourth player
```

This is correct behavior. PlayerC waits. Every chess-server instance runs its own pairing loop (`MATCHMAKING_PAIRING_INTERVAL_MS`, env-configurable — ADR-032/§12) and will match PlayerC when another player arrives.

### Challenge 3: Abandoned Queue Entries

Player enters the queue, then closes the browser or the tab. **Correction from the original version of this section:** Redis sorted sets do not support a per-member TTL — only a whole-key TTL — so "queue entries have a TTL" as originally written here was not actually implementable as described. The actual decided mechanism (ADR-036 §14):

- `matchmaking-service` runs its own periodic sweep (separate ticker from
  chess-server's pairing loop), scanning the queue ZSET for members older
  than `MATCHMAKING_QUEUE_TIMEOUT_SECONDS` (env-configurable), `ZREM`s them,
  and pushes an SSE `MATCHMAKING_FAILED` event with `reason: QUEUE_TIMEOUT`
  if the client is still connected.
- A client-initiated `DELETE /matchmaking/queue` (ADR-038) handles the
  voluntary-cancel case immediately, without waiting for the sweep.
- If another player is matched with an entry whose owner has actually
  disconnected but not yet timed out or cancelled: the resulting game is
  created normally (`CreateMatchedGame` doesn't know or care whether either
  player is currently connected), and the existing WebSocket connect flow's
  ordinary behavior applies — the never-connecting player's side simply
  never sends a first connect. See "Open Question" below for the gap this
  still leaves.

### Challenge 4: Player Already in Queue or Game

A player should not be able to enter the queue if they are already in the queue or in an active game (matchmaking-originated **or** shared-link-originated). Decided mechanism (ADR-037): two independent Redis reads, pipelined, before any enqueue —

- **Already queued:** `ZSCORE queue {userID}` — the queue ZSET itself is the
  source of truth, no separate key.
- **Already has an active game:** `matchmaking_active_game:{userID}`, a
  Redis key written and renewed **only by chess-server** (never by
  matchmaking-service), on the same heartbeat cadence as ownership keys
  (`OwnershipRenewInterval`/`OwnershipTTL`, reused constants). Deliberately a
  heartbeat-renewed lease, not an explicit-clear cache — no delete to miss,
  self-heals via TTL if the owning instance dies. See ADR-037 for the
  rejected alternatives (explicit-clear cache with a reconciliation worker;
  synchronous cross-service REST call to chess-server) and why both lose to
  this.

---

## Queue Data Structure

Redis sorted set: `matchmaking:queue:10+0`

- Member: `userID`
- Score: Unix timestamp of when the player entered the queue (lower score = waited longer)
- **Written by:** `matchmaking-service`, on `POST /matchmaking/queue` (`ZADD ... NX` — idempotent, so a retried/duplicate call never resets queue position)
- **Read/drained by:** every chess-server instance's pairing loop (`ZPOPMIN`), and matchmaking-service's own queue-timeout sweep (`ZREM` on timeout) and cancel handler (`ZREM` on `DELETE`)

```
ZADD matchmaking:queue:10+0 NX 1700000000 "user-abc123"  ←  enqueue (matchmaking-service)
ZPOPMIN matchmaking:queue:10+0 2                          ←  atomic dequeue of 2 (chess-server)
ZREM matchmaking:queue:10+0 "user-abc123"                 ←  leave queue / timeout sweep (matchmaking-service)
ZSCORE matchmaking:queue:10+0 "user-abc123"               ←  check if in queue (matchmaking-service)
ZCARD matchmaking:queue:10+0                               ←  queue depth metric
```

---

## New Endpoints (all served by `matchmaking-service`, behind nginx's shared edge proxy — ADR-035)

**Envelope correction (found during independent review, fixed here):** every
response below now follows `CODING_GUIDELINES.md` §7's envelope exactly
(confirmed directly against `internal/api/response.go`'s `writeData`/
`writeError` — this project's actual, non-negotiable convention, not a
suggestion). The original version of this section showed bare, unwrapped
JSON for every endpoint, and a bare *string* for the 409 error — dropping
the error code entirely, which `writeError`'s signature requires as a
separate field. `matchmaking-service` is its own package/binary, not bound
to chess-server's exact `errorDetail` struct, only to the same shape
convention — its error envelope carries one field beyond `code`/`message`
for the one case that needs it (409 below), everything else matches exactly.

```
POST /matchmaking/queue
  Body: { "userID": "..." }
  Response 200: { "data": { "matchmakingToken": "jwt" } }
  Idempotent: a second call for an already-queued player does not reset
  their position, but still returns a fresh token (ADR-036 §11).
  Response 409 (ADR-037 check-before-enqueue hit):
    { "error": {
        "code": "ALREADY_IN_ACTIVE_GAME",
        "message": "player already has an active game",
        "existingGame": { "gameID": "...", "connectToken": "...",
                           "instanceLabel": "...", "wsPath": "..." }
    } }
  existingGame carries the same fields as a MATCH_FOUND event, nested under
  a named key rather than flattened into the error object — the client
  still never needs a second call or a special case to get connect info out
  of this response, matching ADR-037's original intent; only the wrapping
  changed, not the property.

GET /matchmaking/stream?token=<matchmakingToken>
  SSE. Verifies MatchmakingClaims once at connection open (mirrors WS
  upgrade handling), then holds the connection, pushing MATCH_FOUND /
  MATCHMAKING_FAILED events (ADR-036 §15) as they arrive. SSE event payloads
  are not HTTP request/response pairs and are not subject to the data/error
  envelope above — a stream has one HTTP status for its whole lifetime, not
  one per event; see the Match Notification section below for their actual
  shape, unchanged by this correction.

GET /matchmaking/status?token=<matchmakingToken>
  Same token. Single JSON response, wrapped:
  { "data": { "status": "waiting" } } /
  { "data": { "status": "matched", "gameID": "...", "connectToken": "...",
              "instanceLabel": "...", "wsPath": "..." } } /
  { "data": { "status": "failed", "reason": "..." } }
  Serves both the browser/proxy SSE-fallback case and the lost-RPC/lost-SSE-
  push backstop (ADR-033's Consequences).

DELETE /matchmaking/queue
  Header/token: MatchmakingClaims
  Response 200: { "data": { "status": "dequeued" } }
  Voluntary cancel (ADR-038). Plain synchronous response — no SSE push
  needed, the client already knows the outcome.
```

**Note on the original version of this section:** the endpoint paths above
(`/matchmaking/stream`, `/matchmaking/status`) differ from what an earlier
draft of this document specified (`/matchmaking/queue/status`), and the
response shape of `POST /matchmaking/queue` no longer returns
`{"status":"queued","position":N}` synchronously — position isn't exposed
in the current design. This is the actual decided contract as of ADR-036,
now additionally corrected for envelope conformance above.

---

## Match Notification (SSE, not WebSocket)

**Corrected from the original version of this section**, which described a
WebSocket connection to chess-server for match notification. The decided
design (ADR-036) uses **Server-Sent Events against `matchmaking-service`**
instead, for reasons specific to this codebase: `PlayerClaims` requires a
`GameID` that doesn't exist yet at queue-time, and inventing a placeholder
would corrupt that type's meaning for every other consumer. A new
`MatchmakingClaims{UserID}` JWT type (same signing secret, distinguished by
shape — this codebase's existing one-secret/multiple-claim-shapes
convention) scopes the SSE stream instead.

```
event: MATCH_FOUND
data: {"gameID":"...","connectToken":"...","instanceLabel":"...","wsPath":"/connect/..."}

event: MATCHMAKING_FAILED
data: {"reason":"RETRIES_EXHAUSTED"}   // or "QUEUE_TIMEOUT"
```

`connectToken` is the player's ordinary long-lived `PlayerClaims` — the same
type `/resolve` already issues. **This is deliberate:** after receiving
`MATCH_FOUND`, the client's connect flow is identical to shared-link
ordinary reconnect — dial `/connect/{instanceLabel}` directly, or fall back
through `/resolve` — not a second, matchmaking-specific connection path.
Field names (`connectToken`, `instanceLabel`, `wsPath`) deliberately mirror
`/resolve`'s existing response shape so client-side parsing doesn't need a
special case (confirmed against `resolveResponseData` in
`internal/api/resolve_test.go`).

---

## Matchmaking Loop

A pairing-loop goroutine runs on **each chess-server instance**
(`internal/matchmaking`, new package, chess-server side — not
`matchmaking-service`), on a fixed, env-configurable interval
(`MATCHMAKING_PAIRING_INTERVAL_MS`).

```
Every MATCHMAKING_PAIRING_INTERVAL_MS:
  count = ZCARD matchmaking:queue:10+0
  if count < 2: continue

  pair = ZPOPMIN matchmaking:queue:10+0 2
  if len(pair) < 2: continue  ←  another instance got there first, or queue emptied mid-check

  playerWhiteID = pair[0]
  playerBlackID = pair[1]
  requestID = uuid.New()  // v4 — correlation/idempotency token, not a DB row PK;
                           // see the corrected UUID reasoning in ADR-034

  game, err = Manager.CreateMatchedGame(ctx, playerWhiteID, playerBlackID, requestID)
  // atomic two-player INSERT with ON CONFLICT (matchmaking_request_id) DO NOTHING,
  // local GameRegistry registration, ClaimOwnership — mirrors CreateGame's
  // existing eager pattern

  if err == nil:
    gRPC ReportMatchCreated(gameID, whiteToken, blackToken, instanceLabel) -> matchmaking-service
  else (after bounded retries exhausted):
    ZADD queue original_score playerWhiteID  // re-enqueue, chess-server owns this directly — ADR-034
    ZADD queue original_score playerBlackID
    gRPC ReportMatchmakingFailed(playerWhiteID, playerBlackID, RETRIES_EXHAUSTED) -> matchmaking-service
```

**Important:** `ZPOPMIN` being atomic means only one chess-server instance will successfully dequeue each pair. matchmaking-service is never in this path — see the Architecture Decision section above.

Full contract detail (gRPC methods, retry/dedup semantics, trust boundary): `DECISIONS_LOG_PHASE_3.md` ADR-033, ADR-034; `phases/current/PHASE_3_DESIGN_NOTES.md` §6–§8.

---

## Implementation Checklist

### Step 1: Database — `matchmaking_request_id` idempotency column
- [x] New migration: `matchmaking_request_id UUID UNIQUE` on `games`, nullable (ADR-034)
- [x] `GameStore` gains the atomic two-player insert method `CreateMatchedGame` needs (`ON CONFLICT (matchmaking_request_id) DO NOTHING`)

### Step 2: Proto / gRPC Contract
- [x] `proto/matchmakingv1/matchmaking.proto` (monorepo, ADR-033 §7): `MatchReportService` — `ReportMatchCreated`, `ReportMatchmakingFailed`, `MatchmakingFailureReason` enum
- [x] Codegen wiring for both chess-server (client) and matchmaking-service (server)
- [x] Shared-secret gRPC interceptor (trust boundary, ADR-033)

### Step 3: chess-server side — `internal/matchmaking` package
- [x] `internal/matchmaking/pairing.go`: pairing-loop goroutine, `ZPOPMIN`-driven, `MATCHMAKING_PAIRING_INTERVAL_MS`-configured. `MatchReporter` defined as an interface (not a concrete gRPC type) so the loop's tick logic is fully testable without the generated protobuf stubs, which don't exist on disk yet (`make proto` not yet run) — a concrete gRPC-backed implementation satisfies this interface once the next checklist item lands, with no change to `pairing.go` itself.
- [x] **GATE LIFTED (2026-08-10) — see `DECISIONS_LOG_PHASE_3.md` ADR-041/ADR-042.** `Manager.CreateMatchedGame` (`internal/game`): atomic insert, `GameRegistry` registration, `ClaimOwnership`, PLUS arming `matchedOpponentConnectTimeout` (ADR-042) in the same call — implemented, including the idempotent-retry path (`GameStore.GetGameByMatchmakingRequestID` + `GameRegistry.GetOrHydrate`). Both named prerequisites landed first: `games.activated_at` migration + `GameStore.ActivateGame` (ADR-041), and `internal/game/move.go`'s move-count-gated first-move-timer integration (arm Black's window after move #1, retire + disconnected-player bootstrap after move #2, via `MoveProcessor.onMovePersisted` → `Manager.onMovePersisted`). New, accepted, explicitly-flagged residual gap in the idempotent-retry branch: it does not re-arm `matchedOpponentConnectTimeout` or re-claim ownership, on the assumption neither step could plausibly fail independently of the insert itself — see `Manager.CreateMatchedGame`'s doc comment.
- [x] Re-enqueue-on-failure path (`ZADD` at original score, chess-server-direct, ADR-034) — `PairingLoop.reenqueue`, part of the same commit as `pairing.go` above (the tick logic's failure branch requires it; they were never separable in practice)
- [x] gRPC client: `ReportMatchCreated`/`ReportMatchmakingFailed`, bounded retry with backoff — `internal/matchmaking/reporter.go`'s `grpcMatchReporter`, using `internal/rpc`'s shared-secret client interceptor. Wired into `cmd/server/main.go` (optional — gated on `MATCHMAKING_SERVICE_ADDR` being set).
- [x] `matchmaking_active_game:{userID}` key: write at `CreateGame`/`JoinGame`/`CreateMatchedGame` success, renew on the existing heartbeat tick (ADR-037) — extends `internal/game/heartbeat.go`, not a new goroutine. `RoutingDirectory.SetActiveGameMarker`/`RenewActiveGameMarkersBatch` (`directory.go`); renewal re-signs a fresh `PlayerClaims` token per tick rather than threading the original token through `GameSession` (see `renewActiveGameMarkers`'s doc comment for why — `JoinGame` never touches a live `GameSession`, so a session-held-token design has no way to populate Black's token in that case).
- [x] Redis client wiring decision for `internal/matchmaking`: reuse `main.go`'s existing `*redis.Client` (same process as `internal/game`) rather than opening a second connection — implementation-time detail, not requiring its own ADR. `NewPairingLoop`'s signature enforces this directly (takes an already-connected `*redis.Client`, never constructs its own).
- [~] Integration tests (Redis + Postgres required): concurrent `ZPOPMIN` across two simulated instances, no double-match ✅ (`TestPairingLoop_ConcurrentTicks_NoDoubleMatch`); `CreateMatchedGame` idempotency under retry ✅ (Step 1's `TestGameStore_CreateMatchedGame`, plus `pairing_test.go`'s pairing/odd-queue/re-enqueue-on-failure coverage). Not yet covered here, deliberately — depends on `matchmaking-service` (Step 4) existing: the full queue→SSE→connect end-to-end flow, and the 409-already-in-a-game check. See Step 7 below.

### Step 4: `matchmaking-service` — new deployable
- [x] New Go module/binary (monorepo, own `docker-compose.yml` entry per ADR-032's Consequences) — `cmd/matchmaking-service/main.go` (config, Redis connectivity via `internal/mmsvc.NewRedisClient` — deliberately duplicated rather than importing `internal/game`, to avoid pulling chess-server's game package into this binary — HTTP server, graceful shutdown), `Dockerfile.matchmaking-service`, `docker-compose.yml`'s `matchmaking-service` entry (port 8081, matching `nginx.conf`'s existing assumption; no `DATABASE_URL` — this service never talks to Postgres). `nginx`'s `depends_on` deliberately left unchanged (not extended to this service) — its `/matchmaking` location block's resolver-based dynamic `proxy_pass` exists specifically so nginx's own startup and the other two locations stay independent of this service's health.
- [x] `POST /matchmaking/queue`: mint `MatchmakingClaims`, `ZADD ... NX`, check-before-enqueue — `internal/mmsvc` (`queue.go`, `handler.go`, `router.go`, `response.go`). Implements the ADR-037 active-game-marker read (409 `ALREADY_IN_ACTIVE_GAME`) before any write; deliberately does **not** perform a separate `ZSCORE`-for-"already queued" read ahead of the write — `ZADD NX` already makes the enqueue itself idempotent without it, so a second read would be a redundant round trip, not additional correctness (see `Queue.Enqueue`'s doc comment). `MatchmakingClaims`/`MatchmakingClaimsTTL`/`SignMatchmakingToken`/`VerifyMatchmakingToken` added to `internal/auth/token.go` (this item and the one below landed together — the endpoint cannot mint a token without the claims type existing).
- [x] `GET /matchmaking/stream`: SSE, `MatchmakingClaims`-authenticated, in-memory `userID -> flusher` map (M=1 only, see ADR-036's scaling caveat) — `internal/mmsvc/hub.go` (`Hub`: register/unregister/Notify, buffered per-user event channel, pointer-identity guard against a reconnect race deleting a newer registration) and `handler.go`'s `Handler.Stream` (verifies `MatchmakingClaims` once at connection open, relays Hub events as SSE frames until client disconnect). `EventMatchFound`/`EventMatchmakingFailed`/`matchFoundData`/`matchmakingFailedData` defined now, unused until the gRPC server (below) becomes the first producer calling `Hub.Notify` — the connection lifecycle itself (register → relay → clean unregister on `ctx.Done()`) is independently complete and testable without a producer existing yet.
- [x] `GET /matchmaking/status`: polling/backstop endpoint, same token — `Queue.GetResult`/`Queue.SetResult` (`matchmaking_result:{userID}` key, 5-minute TTL) and `Handler.Status` in `internal/mmsvc`. "waiting" is the absence of a result record, not a distinct tracked state; `SetResult` is unused until the gRPC server (below) becomes its first caller, same deliberate incompleteness as `Hub.Notify`.
- [x] `DELETE /matchmaking/queue`: voluntary cancel (ADR-038) — `Queue.Dequeue` (`ZREM`, idempotent by nature of the operation) and `Handler.Cancel`. PHASE_3.md's endpoint listing was ambiguous on transport ("Header/token") for this one — resolved in favor of the same `?token=` query convention `Stream`/`Status` already need (unavoidable for `Stream`, since browser `EventSource` cannot set custom headers), rather than introducing a second scheme for the same token type. All three of `Stream`/`Status`/`Cancel` now share one `verifyMatchmakingTokenFromQuery` helper.
- [x] Queue-timeout sweep goroutine (`MATCHMAKING_QUEUE_TIMEOUT_SECONDS`, ADR-036 §14) — `internal/mmsvc/sweep.go`'s `Sweep` (own 5s ticker, separate from the pairing loop's, per ADR-036's "separate ticker" instruction), `ZRANGEBYSCORE`/`ZREM` against the queue ZSET, `Queue.SetResult` + `Hub.Notify` with reason `QUEUE_TIMEOUT` — the one `MatchmakingFailureReason` value generated natively rather than translated from the gRPC enum. **Default timeout (30s) is a judgment call, not sourced from `PHASE_3_DESIGN_NOTES.md` §14** (not loaded this session) — flagged in `main.go`'s `defaultQueueTimeoutSeconds` doc comment for revisiting if that document specifies a different number.
- [x] gRPC server: `MatchReportService` implementation, `reported:<gameID>` dedup key (short TTL) before SSE push — `internal/mmsvc/reportserver.go`'s `ReportServer` (embeds `UnimplementedMatchReportServiceServer` by value). Confirmed against `DECISIONS_LOG_PHASE_3.md` ADR-037's Consequences before implementing: `white_token`/`black_token` are the players' existing 24h `PlayerClaims` (not `ConnectClaims`, despite the `connectToken` field name matching `/resolve`'s response shape) — `ReportServer` treats them as opaque strings, never parsing or verifying them, relaying them unchanged into both `Queue.SetResult` and the `MATCH_FOUND` SSE event. `ReportMatchmakingFailed` has no dedup key, matching ADR-033's Consequences exactly ("no natural key... accepted at-least-once"). Wired into `cmd/matchmaking-service/main.go` behind `internal/rpc.NewUnaryServerAuthInterceptor` on its own listener (port 9090, `MATCHMAKING_SVC_GRPC_PORT`, distinct from the HTTP/SSE port 8081) — `docker-compose.yml`'s `server1`/`server2` now also set `MATCHMAKING_SERVICE_ADDR: matchmaking-service:9090` and `MATCHMAKING_SHARED_SECRET`, previously unset since nothing on the receiving end existed yet.

**Step 4 complete — all 8 checklist items done.**
- [x] `MatchmakingClaims`/`MatchmakingClaimsTTL` in `internal/auth` — landed as part of the `POST /matchmaking/queue` item above. **TTL still hardcoded at 10s**, not yet env-configurable — deliberately not done here; folded into Step 5 below rather than duplicating that work across two checklist items.

### Step 5: `ConnectClaimsTTL` — make configurable (fix identified during independent review)
- [x] `ConnectClaimsTTL` was hardcoded `60 * time.Second` in `internal/auth/token.go`, with a doc comment claiming "(10s)" — the constant had been changed locally for manual-testing convenience and never reverted. Fixed: renamed `DefaultConnectClaimsTTL`/`DefaultMatchmakingClaimsTTL` (now only fallback defaults, corrected to actually be 10s), with the enforced value moved to a field on `internal/game.Manager` / `internal/mmsvc.Handler` respectively, injected via their constructors and resolved from `CONNECT_CLAIMS_TTL_SECONDS`/`MATCHMAKING_CLAIMS_TTL_SECONDS` (both optional, default 10) in `cmd/server/main.go`/`cmd/matchmaking-service/main.go` — not a package-level mutable var read directly at the mint site, which `CODING_GUIDELINES.md` §5 forbids. `NewManager`'s signature change (10th parameter) broke 6 integration-tagged test call sites `gopls:go_symbol_references` could not see (`//go:build integration` files are invisible to it, a known limitation) — found and fixed by manually checking every test file in `internal/game`, `internal/api`, and `internal/matchmaking` rather than trusting the tool's reference count.

### Step 6: nginx
- [x] New `location /matchmaking { ... }` block: `proxy_buffering off`, `proxy_read_timeout 3600s` (ADR-035, ADR-036)

### Step 7: Integration Testing
- [ ] Two players enter queue → both receive `MATCH_FOUND` via SSE → game connects normally through the existing resolve/connect flow
- [ ] Three players enter queue → first two matched, third waits
- [ ] Player enters queue then disconnects before the sweep fires → cancels cleanly if `DELETE` is called, or times out via the sweep otherwise
- [ ] Simulate two chess-server instances running pairing loops simultaneously → no player double-matched
- [ ] `CreateMatchedGame` insert failure (simulated) → re-enqueue at original score, `ReportMatchmakingFailed` fires, player sees `MATCHMAKING_FAILED` via SSE
- [ ] Player already in an active shared-link game attempts to queue → 409 with existing game's connect info, not silently re-enqueued

### Step 8: Documentation
- [x] Reconcile this document against ADR-032–038 (this pass)
- [x] Update `ARCHITECTURE.md`: matchmaking-service component, dependency graph, database schema addition (this pass adds a design-decided-not-yet-implemented section; final reconciliation happens once implementation lands, same pattern as Phase 2's ARCHITECTURE.md note)
- [x] Document the rejected in-process-pub/sub alternative — done as `DECISIONS_LOG_PHASE_3.md` ADR-040 (a new entry, not an edit to ADR-032 itself, per this project's append-only discipline)
- [x] Correct ADR-034's UUID-convention rationale — done as ADR-039 (decision unchanged, cited justification retracted)
- [x] Fix response envelope conformance in this document's endpoint examples (`CODING_GUIDELINES.md` §7 vs. `internal/api/response.go` — found during independent review; every example in "New Endpoints" above now wrapped correctly)
- [ ] Update `CLAUDE.md`

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Two players entering the queue are matched and receive `MATCH_FOUND` via SSE |
| 2 | A player cannot be matched with themselves |
| 3 | A player who disconnects while queued and never returns is removed from the queue within `MATCHMAKING_QUEUE_TIMEOUT_SECONDS` |
| 4 | Simulate two concurrent chess-server pairing loops: no player is ever matched twice |
| 5 | Queue depth is correctly reported and measurable (`ZCARD`) |
| 6 | A player already in an active game (matchmaking- or shared-link-originated) cannot be enqueued |
| 7 | A `CreateMatchedGame` insert failure results in a clean re-enqueue at original queue position, not a lost or duplicated player |
| 8 | All Phase 1 and 2 acceptance criteria still pass |
| 9 | `go test -race ./...` passes across both chess-server and matchmaking-service |

---

## Technical Debt This Phase May Introduce

| ID | Description | Must Fix By |
|----|-------------|-------------|
| TD-P3-001 | FIFO queue only — no ELO-based pairing | Phase 4 (ELO exists then) |
| TD-P3-002 | Single time control (10+0) only | Phase 4 |
| TD-P3-003 | `matchmaking-service`'s SSE connections live in an in-memory map, correct only at one replica (M=1). Scaling matchmaking-service itself beyond one replica reintroduces the same delivery-affinity problem this phase extracted matchmaking out of chess-server to avoid, at a smaller scale (ADR-036). | Revisit if/when matchmaking-service needs to scale, or at Phase 5 (spectator fan-out) if that phase's mechanism ends up related |
| TD-P3-004 | **CLOSED (2026-08-10, `DECISIONS_LOG_PHASE_3.md` ADR-041/ADR-042).** A matched player who connects and stays connected while their assigned opponent never connects at all now has a dedicated arm-at-creation-time timeout (`matchedOpponentConnectTimeout`, ADR-042) that fires regardless of any future disconnect/reconnect/resolve event, closing the steady-state gap unconditionally. Working through this surfaced a second, related but structurally distinct gap not originally named in this row — a game that reaches `ACTIVE` but never receives a first move, or a first reply — closed separately by ADR-041's move-triggered first-move grace period (new `ACTIVE→ABORTED` edge, scoped universally, not matchmaking-only). One accepted residual gap remains, of the same shape and severity as the already-tracked TD-P2-006 (a game nobody ever connects to or resolves, combined with an instance crash before the creation-armed timer fires): not solved here, deliberately, per ADR-042's Rationale (a heartbeat-tick alternative was traced through and shown to provide zero additional coverage for this specific residual case). | Resolved — see ADR-041/ADR-042 for design, `phases/current/PHASE_3_DESIGN_NOTES.md` §17 for full reasoning trail |

---

## Matched-Game Connection-Grace Semantics — RESOLVED (2026-08-10)

**Status: closed.** `DECISIONS_LOG_PHASE_3.md` ADR-041 and ADR-042 resolve
this in full; full reasoning trail in `phases/current/PHASE_3_DESIGN_NOTES.md`
§17. Summary, so this section still stands on its own:

**Resolved, first (2026-08-08), re-verified fresh rather than cited
(2026-08-10):** if a matched player connects and their opponent never
does, and the *connected* player then also disconnects, the existing
mechanism handles this correctly with no new code — `onAbandonTimeout`
branches purely on `snap.Status == store.GameStatusWaiting`, with no
dependency on how the game was created. Re-confirmed by direct re-read of
`HandleConnect`/`HandleDisconnect`/`onAbandonTimeout`, not assumed from
this document's prior description of the trace.

**Resolved (2026-08-10), ADR-042:** a matched player who connects and
stays connected — waiting — while their assigned opponent never connects
at all now has a dedicated mechanism: a per-process timer armed
synchronously at `CreateMatchedGame` time (`matchedOpponentConnectTimeout`,
60s), cancelled the moment the opponent actually connects, firing straight
to `ABORTED` (reusing the existing `WAITING→ABORTED` edge) if they never
do. Crash+failover with at least one party later resolving is already
covered as a side effect of `DECISIONS_LOG_PHASE_2.md` ADR-031's existing
disconnect-timestamp fallback (traced through concretely in ADR-042's
Context, not assumed). Crash+nobody-ever-resolves remains an accepted,
bounded residual gap of the same shape as the already-tracked TD-P2-006 —
not solved here, deliberately (see ADR-042's Rationale for why a
heartbeat-tick alternative doesn't actually close it either).

**Found while resolving the above, not originally part of this question:**
a second, structurally distinct gap — a game that reaches `ACTIVE` (both
players connected) but never receives a first move, or receives White's
first move with no reply from Black — had no termination mechanism at all,
and the existing disconnect-triggered timer would in fact score it
misleadingly (a `COMPLETED` win or `ABANDONED` draw for a game with zero
real chess played) on the rare occasion it did fire. Closed by ADR-041's
move-triggered first-move grace period, scoped universally (shared-link
games too, not matchmaking-only) since the underlying defect is not
matchmaking-specific — flagged there as a material behavior change to
already-shipped Phase 1/2 code, worth explicit attention during regression
testing.

`CreateMatchedGame`'s connection lifecycle is no longer undesigned — see
Step 3's checklist entry above for what implementing it now requires.
