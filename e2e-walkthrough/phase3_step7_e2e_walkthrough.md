# Phase 3 Step 7 — Matchmaking E2E Testing Walkthrough

Companion to `phase2_step11_e2e_walkthrough.md`, same spirit: manual,
real-process testing against the actual cluster — now including
`matchmaking-service` as a real container, not just `server1`/`server2` —
not unit/integration tests. This document is the step-by-step script for
PHASE_3.md's Step 7 checklist.

**Prerequisites:**
- `.env` filled in (copy from `.env.example` if you haven't) — this now
  needs `MATCHMAKING_SHARED_SECRET` set to a real value; every scenario
  below fails at the gRPC layer without it.
- `jq`, `curl`, `uuidgen`, and `wscat` available (`npm install -g wscat`,
  or use `npx wscat` inline — the commands below use `npx wscat` so
  nothing needs global install).
- Nothing else already running on ports 5432, 6379, 8080.

**A real gap this walkthrough depends on being fixed, not a test-setup
detail:** `POST /matchmaking/queue` can never create a `users` row —
`matchmaking-service` never touches Postgres at all
(`ARCHITECTURE.md`'s Matchmaking section). `CreateMatchedGame`'s `INSERT`
carries an FK constraint on `users`, so a `userID` that has never gone
through `/games` at least once fails to match, deterministically,
indistinguishable from a genuine `CreateMatchedGame` failure. `POST /users`
(`DECISIONS_LOG_PHASE_3.md` ADR-043, fixes TD-P3-007) exists specifically to
seed a real `users` row without going through game creation first — every
"happy path" scenario below registers via `POST /users` before queueing.
Deliberately minimal by explicit instruction: it returns a bare `userID`,
no token, no session, no expiration — you use that `userID` exactly as you
would an anonymously-generated one, everywhere else in the API.

Start the full stack:

```bash
make docker-up-cluster
docker compose ps
```

You should now see `chess_postgres_db`, `chess_redis`, `chess_migrate`
(exits `0` — one-shot, not a bug), `chess_server1`, `chess_server2`,
`chess_matchmaking_service`, `chess_nginx`, all healthy. If
`chess_matchmaking_service` never reaches healthy, check
`docker compose logs matchmaking-service` for a config error first — its
`loadConfig` panics loudly on a missing `MATCHMAKING_SHARED_SECRET` or
`JWT_SECRET`, so a crash-looping container almost always means one of those
two is unset in `.env`.

Useful commands you'll reuse throughout:

```bash
# Watch matchmaking-service's own logs — pairing decisions, dedup hits,
# sweep removals, gRPC report handling all log here, distinct from
# server1/server2's own logs.
docker compose logs -f matchmaking-service

# Watch either chess-server instance's pairing-loop activity
docker compose logs -f server1
docker compose logs -f server2

# Inspect matchmaking's Redis state directly, at any point
docker compose exec redis redis-cli ZRANGE 'matchmaking:queue:10+0' 0 -1 WITHSCORES
docker compose exec redis redis-cli KEYS 'matchmaking_active_game:*'
docker compose exec redis redis-cli KEYS 'matchmaking_result:*'
docker compose exec redis redis-cli KEYS 'reported:*'
```

**Shortcut throughout:** every scenario below has an automated equivalent in
`e2e-phase3.sh` (`./e2e-phase3.sh scenario1` etc.) that runs the same steps
with less typing. The manual commands are spelled out here so you can watch
each response shape yourself the first time through; reach for the script
once you trust the flow and just want to re-run it.

---

## Scenario 1: Two players queue, get matched, connect directly, then reconnect via `/resolve`

The foundational matchmaking property everything else depends on: two
independent queue calls converge onto one shared game, exactly the way
Phase 2's Scenario 0 established co-location for shared-link games. Also
exercises both connect paths `DECISIONS_LOG_PHASE_3.md`/`PHASE_3_DESIGN_NOTES.md`
§18 (2026-08-17) establishes: the initial match delivers a directly-dialable
token, and a later reconnect still goes through `/resolve` exactly like an
ordinary shared-link game.

```bash
# 1. Register two real users (fixes TD-P3-007 — see this doc's header)
curl -s -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"alice_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
  | tee /tmp/alice.json | jq .
ALICE_ID=$(jq -r '.data.userID' /tmp/alice.json)

curl -s -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"bob_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
  | tee /tmp/bob.json | jq .
BOB_ID=$(jq -r '.data.userID' /tmp/bob.json)

# 2. Both queue for matchmaking
curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$ALICE_ID\"}" \
  | tee /tmp/alice_queue.json | jq .
ALICE_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/alice_queue.json)

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$BOB_ID\"}" \
  | tee /tmp/bob_queue.json | jq .
BOB_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/bob_queue.json)

# 3. Open SSE streams for both (background — these are one-directional,
#    server->client only, unlike the WebSocket connections below)
curl -N -s "http://localhost:8080/matchmaking/stream?token=$ALICE_MM_TOKEN" > /tmp/alice.sse.log &
curl -N -s "http://localhost:8080/matchmaking/stream?token=$BOB_MM_TOKEN" > /tmp/bob.sse.log &
```

Wait a few seconds for the pairing loop to tick (`MATCHMAKING_PAIRING_INTERVAL_MS`,
default 500ms) and the gRPC round trip to complete, then:

```bash
grep -A1 'event: MATCH_FOUND' /tmp/alice.sse.log
grep -A1 'event: MATCH_FOUND' /tmp/bob.sse.log
```

**Watch for:** both show a `MATCH_FOUND` event with a `data:` line
containing `gameID`/`connectToken`/`playerToken`/`instanceLabel`/`wsPath` —
two distinct tokens now, not one (`PHASE_3_DESIGN_NOTES.md` §18) — and
**the `gameID` must be identical** between Alice's and Bob's frames. If it
isn't, stop here; that's a matching-integrity bug, not something to shrug
off.

```bash
ALICE_DATA=$(grep -A1 'event: MATCH_FOUND' /tmp/alice.sse.log | tail -n1 | sed 's/^data: //')
BOB_DATA=$(grep -A1 'event: MATCH_FOUND' /tmp/bob.sse.log | tail -n1 | sed 's/^data: //')

ALICE_CONNECT=$(echo "$ALICE_DATA" | jq -r '.connectToken')
ALICE_PLAYER_TOKEN=$(echo "$ALICE_DATA" | jq -r '.playerToken')
ALICE_WS_PATH=$(echo "$ALICE_DATA" | jq -r '.wsPath')
BOB_CONNECT=$(echo "$BOB_DATA" | jq -r '.connectToken')
BOB_PLAYER_TOKEN=$(echo "$BOB_DATA" | jq -r '.playerToken')
BOB_WS_PATH=$(echo "$BOB_DATA" | jq -r '.wsPath')
GAME_ID=$(echo "$ALICE_DATA" | jq -r '.gameID')

echo "Alice dials: ws://localhost:8080${ALICE_WS_PATH}?token=$ALICE_CONNECT"
echo "Bob dials:   ws://localhost:8080${BOB_WS_PATH}?token=$BOB_CONNECT"
```

**`connectToken` here is directly dialable — no `/resolve` call needed for
this first connection.** `CreateMatchedGame` minted it fresh, synchronously,
on the exact instance that owns the new game (`PHASE_3_DESIGN_NOTES.md`
§18.1's trace: zero elapsed time between minting and delivery means zero
staleness risk, unlike the active-game marker's `playerToken`). Dial
promptly — it's short-lived (`ConnectClaimsTTL`, default 30s):

```bash
# Terminal 1 (Alice)
npx wscat -c "ws://localhost:8080${ALICE_WS_PATH}?token=$ALICE_CONNECT"

# Terminal 2 (Bob)
npx wscat -c "ws://localhost:8080${BOB_WS_PATH}?token=$BOB_CONNECT"
```

**Watch for:** both connect successfully. The very first `GAME_STATE` each
receives should show `status: "WAITING_FOR_PLAYER"` — **not** `"ACTIVE"`
immediately, even though both players are already matched. This is
`CreateMatchedGame`'s deliberate design (`TD-P3-004`/ADR-041/042): the game
starts `WAITING_FOR_PLAYER` the same as an ordinary shared-link game, and
only flips to `ACTIVE` once **both** players have actually connected over
WebSocket — matching is not the same event as both players being present.
Once both wscat sessions are open, you should see a second `GAME_STATE` (or
`OPPONENT_CONNECTED`) on the first-connected side showing the flip to
`ACTIVE`.

Send a move from whichever side connected first:

```json
{"type":"MOVE","san":"e4"}
```

**Watch for:** `MOVE_APPLIED` delivered to the other terminal — confirms
real co-location on the same live `GameSession`, not just a matching
`gameID` on paper.

### Now the other half: reconnect via `/resolve`

Close Alice's terminal (Ctrl-C or kill the `wscat` process), simulating a
dropped connection, then reconnect using her `playerToken` — the fallback
path `PHASE_3_DESIGN_NOTES.md` §18.2 requires alongside the fast path,
identical to an ordinary shared-link reconnect:

```bash
ALICE_RESOLVE=$(curl -s "http://localhost:8080/games/$GAME_ID/resolve" \
  -H "Authorization: Bearer $ALICE_PLAYER_TOKEN")
ALICE_RECONNECT=$(echo "$ALICE_RESOLVE" | jq -r '.data.connectToken')
ALICE_RECONNECT_WS_PATH=$(echo "$ALICE_RESOLVE" | jq -r '.data.wsPath')

npx wscat -c "ws://localhost:8080${ALICE_RECONNECT_WS_PATH}?token=$ALICE_RECONNECT"
```

**Watch for:** Alice reconnects cleanly and receives a current `GAME_STATE`
showing `e4` already played — confirms `/resolve`'s fallback path is
unaffected by any of §18's changes to the initial-match fast path.

---

## Scenario 2: Three players queue — first two matched, third waits

Confirms the pairing loop's "two lowest scores, earliest arrivals" rule
(PHASE_3.md's Key Technical Challenges) actually holds under a real, odd
queue depth.

```bash
for name in carol dave erin; do
  curl -s -X POST http://localhost:8080/users \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"${name}_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
    > /tmp/${name}.json
  eval "${name^^}_ID=\$(jq -r '.data.userID' /tmp/${name}.json)"
done

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$CAROL_ID\"}" | jq .
curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$DAVE_ID\"}" | jq .
curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$ERIN_ID\"}" \
  | tee /tmp/erin_queue.json | jq .
ERIN_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/erin_queue.json)
```

Wait a few seconds, then:

```bash
curl -s "http://localhost:8080/matchmaking/status?token=$ERIN_MM_TOKEN" | jq .
docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'
```

**Watch for:** Erin's status is `"waiting"` (queued last, correctly not
part of the first pair), and the queue depth is `1` — just Erin. If the
queue depth is `0` or `3`, something paired the wrong two players or paired
nobody at all — stop and check `docker compose logs` on both `server1` and
`server2` for pairing-loop errors.

---

## Scenario 3a: Player queues then voluntarily cancels

```bash
curl -s -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"frank_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
  | tee /tmp/frank.json | jq .
FRANK_ID=$(jq -r '.data.userID' /tmp/frank.json)

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$FRANK_ID\"}" \
  | tee /tmp/frank_queue.json | jq .
FRANK_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/frank_queue.json)

docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'

curl -s -X DELETE "http://localhost:8080/matchmaking/queue?token=$FRANK_MM_TOKEN" | jq .

docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'
curl -s "http://localhost:8080/matchmaking/status?token=$FRANK_MM_TOKEN" | jq .
```

**Watch for:** queue depth drops by exactly one after the `DELETE`, and the
status call afterward still shows `"waiting"` — **not** an error, and
**not** `"failed"`. `Handler.Cancel` deliberately writes no result record
(see its own doc comment) — `"waiting"` here just means "no result exists,"
which remains true after a clean voluntary cancel.

---

## Scenario 3b: Player queues, never cancels, sweep times them out

A real wall-clock wait — same discipline as Phase 2's Scenario 3/6:
`MATCHMAKING_QUEUE_TIMEOUT_SECONDS` (default 30s) plus one sweep tick
(`internal/mmsvc/sweep.go`'s fixed 5s interval). Budget ~40 seconds, don't
rush it.

```bash
curl -s -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"grace_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
  | tee /tmp/grace.json | jq .
GRACE_ID=$(jq -r '.data.userID' /tmp/grace.json)

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$GRACE_ID\"}" \
  | tee /tmp/grace_queue.json | jq .
GRACE_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/grace_queue.json)

curl -N -s "http://localhost:8080/matchmaking/stream?token=$GRACE_MM_TOKEN" > /tmp/grace.sse.log &

docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'
sleep 40
docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'

grep -A1 'event: MATCHMAKING_FAILED' /tmp/grace.sse.log
curl -s "http://localhost:8080/matchmaking/status?token=$GRACE_MM_TOKEN" | jq .
```

**Watch for:** queue depth drops back to whatever it was before Grace
queued, her SSE log shows `event: MATCHMAKING_FAILED` with
`"reason":"QUEUE_TIMEOUT"`, and her status endpoint independently agrees
(`status: "failed", reason: "QUEUE_TIMEOUT"`) — the SSE push and the
polling backstop must tell the same story, since `GET /matchmaking/status`
exists specifically to be reliable even if the SSE push were somehow missed
(`DECISIONS_LOG_PHASE_3.md` ADR-033's Consequences).

---

## Scenario 4: Concurrent pairing loops — no double-match under real load

Phase 3's actual core learning objective, exercised for real: both
`server1` and `server2` run independent pairing-loop tickers
(`MATCHMAKING_PAIRING_INTERVAL_MS`, default 500ms each), racing each other's
`ZPOPMIN` against the same shared queue over the real network — not the
in-process simulation `internal/matchmaking/pairing_test.go`'s
`TestPairingLoop_ConcurrentTicks_NoDoubleMatch` already covers under
`-race`.

```bash
docker compose exec -T postgres psql -U chess -d chess -tAc \
  "SELECT COUNT(*) FROM games WHERE matchmaking_request_id IS NOT NULL"
# note this as $BEFORE
```

Queue 10 pairs (20 players) as fast as your shell can issue the calls —
`./e2e-phase3.sh scenario4 10` does exactly this loop if you'd rather not
type 20 `curl` calls by hand; the manual version is the same
`POST /users` + `POST /matchmaking/queue` pair, repeated:

```bash
for i in $(seq 1 20); do
  curl -s -X POST http://localhost:8080/users \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"race${i}_$(uuidgen | cut -c1-8)\",\"password\":\"correct-horse-battery\"}" \
    > /tmp/race_user_$i.json
  UID=$(jq -r '.data.userID' /tmp/race_user_$i.json)
  curl -s -X POST http://localhost:8080/matchmaking/queue \
    -H "Content-Type: application/json" -d "{\"userID\":\"$UID\"}" > /dev/null &
done
wait
```

Wait for the queue to drain, then:

```bash
docker compose exec redis redis-cli ZCARD 'matchmaking:queue:10+0'
# want: 0

docker compose exec -T postgres psql -U chess -d chess -tAc \
  "SELECT COUNT(*) FROM games WHERE matchmaking_request_id IS NOT NULL"
# note this as $AFTER — want exactly $BEFORE + 10
```

**Watch for:** exactly 10 new matched games, no more, no fewer. More than
10 means two instances both matched the same pair independently (the exact
double-match race this phase's Redis `ZPOPMIN` atomicity exists to
prevent); fewer than 10 means a pair was silently lost to an uncaught
failure somewhere. Either outcome is a stop-and-investigate finding, not
something to re-run hoping for a cleaner result.

---

## Scenario 5: `CreateMatchedGame` insert failure — re-enqueue and `MATCHMAKING_FAILED`

Deliberately **skips** `POST /users` this time, to exploit the exact FK
gap this document's header describes on purpose — mirrors
`internal/matchmaking/pairing_test.go`'s
`TestPairingLoop_Tick_CreateMatchedGameFails_ReenqueuesAndReportsFailure`,
but against the real cluster.

```bash
HENRY_ID=$(uuidgen)   # never registered — no users row exists for this ID
IVY_ID=$(uuidgen)

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$HENRY_ID\"}" \
  | tee /tmp/henry_queue.json | jq .
HENRY_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/henry_queue.json)

curl -s -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$IVY_ID\"}" \
  | tee /tmp/ivy_queue.json | jq .
IVY_MM_TOKEN=$(jq -r '.data.matchmakingToken' /tmp/ivy_queue.json)

curl -N -s "http://localhost:8080/matchmaking/stream?token=$HENRY_MM_TOKEN" > /tmp/henry.sse.log &

sleep 5
grep -A1 'event: MATCHMAKING_FAILED' /tmp/henry.sse.log
```

**Watch for:** `event: MATCHMAKING_FAILED` with `"reason":"RETRIES_EXHAUSTED"`
— the gRPC-reported kind (`translateFailureReason`'s mapping in
`reportserver.go`), **not** `QUEUE_TIMEOUT` (Scenario 3b's sweep-generated
reason; the two must never be confused, since they imply very different
things to a client deciding whether to just re-queue automatically).

**A real, observable finding, not a script artifact:** `PairingLoop.reenqueue`
puts both players back at their *original* queue scores after this failure
(ADR-034), and their underlying problem — no `users` row — is permanent,
not transient. Left alone, the very next pairing-loop tick (500ms later)
immediately re-picks them and fails again, in an unbounded retry loop with
no backoff. Confirm this yourself before cleaning up:

```bash
sleep 2
docker compose exec redis redis-cli ZSCORE 'matchmaking:queue:10+0' "$HENRY_ID"
# still present — re-enqueued, not dropped
```

Then break the loop:

```bash
curl -s -X DELETE "http://localhost:8080/matchmaking/queue?token=$HENRY_MM_TOKEN" > /dev/null
curl -s -X DELETE "http://localhost:8080/matchmaking/queue?token=$IVY_MM_TOKEN" > /dev/null
```

There is currently no distinction anywhere in this system between a
*transient* `CreateMatchedGame` failure (worth retrying) and a
*deterministic* one (will never succeed, should stop retrying and surface
differently) — worth its own tech-debt entry if you haven't already
recorded one.

---

## Scenario 6: Already in an active shared-link game — 409, not silently queued

```bash
JACK_ID=$(uuidgen)
curl -s -X POST http://localhost:8080/games \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$JACK_ID\"}" | tee /tmp/create.json | jq .
GAME_ID=$(jq -r '.data.gameID' /tmp/create.json)
```

```bash
curl -s -w "\nHTTP %{http_code}\n" -X POST http://localhost:8080/matchmaking/queue \
  -H "Content-Type: application/json" -d "{\"userID\":\"$JACK_ID\"}" | jq .
```

**Watch for:** `HTTP 409`, `error.code: "ALREADY_IN_ACTIVE_GAME"`,
`error.existingGame.gameID` matching `$GAME_ID`. No wait needed before this
call — the active-game marker is written *eagerly* at `CreateGame` itself
(PHASE_3.md Step 3's checklist), before any opponent ever joins as Black.

```bash
docker compose exec redis redis-cli ZSCORE 'matchmaking:queue:10+0' "$JACK_ID"
```

**Watch for:** `(nil)` — confirms Jack was rejected outright, not
enqueued-then-rejected (which would leave a spurious queue entry to clean
up — `Handler.Queue`'s doc comment specifically calls out this ordering as
load-bearing).

---

## Wrap-up

```bash
docker compose down   # stop everything; add -v to also wipe volumes
```

Same discipline as Phase 2's walkthrough: if any scenario diverges from
"Watch for," that's a real bug to bring back for review, not something to
paper over or re-run hoping for a different result. If reality disagrees
with what `PHASE_3.md`/the ADRs predicted, the design docs get corrected to
match what's actually true, not the other way around.

For each scenario, note in `CLAUDE.md`'s session log (or wherever you're
tracking this) whether it passed as described, before marking PHASE_3.md's
Step 7 checklist item complete.
