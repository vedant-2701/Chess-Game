# Phase 2 Step 11 — Multi-Instance E2E Testing Walkthrough

Companion to `phase1_step14_e2e_walkthrough.md`, same spirit: manual,
real-process testing against the actual cluster, not unit/integration tests.
This document is the step-by-step script for PHASE_2.md's Step 11 checklist.

**Prerequisites:**
- `.env` filled in (copy from `.env.example` if you haven't).
- `jq`, `curl`, and `wscat` available (`npm install -g wscat`, or use `npx
  wscat` inline — the commands below use `npx wscat` so nothing needs global
  install).
- Nothing else already running on ports 5432, 6379, 8080.

Start the full stack:

```bash
make docker-up-cluster
docker compose ps
```

You should see `chess_postgres_db`, `chess_redis`, `chess_migrate` (exits
`0` — that's correct, it's one-shot, not a bug), `chess_server1`,
`chess_server2`, `chess_nginx`, all healthy. If `chess_migrate` shows a
non-zero exit or `server1`/`server2` never reach healthy, stop here —
Step 11 assumes Steps 1–10 are already solid.

Useful commands you'll reuse throughout:

```bash
# Watch one instance's logs live — the slog messages this project logs
# throughout (game hydrated, heartbeat renewals, zombie corrections, etc.)
# are your main signal for "what actually happened," not just HTTP status
# codes.
docker compose logs -f server1
docker compose logs -f server2

# Inspect the routing directory directly, at any point
docker compose exec redis redis-cli KEYS 'game:*'
docker compose exec redis redis-cli KEYS 'instance_alive:*'
docker compose exec redis redis-cli GET game:<gameID>
```

---

## Scenario 0: Co-location — two players always land on the same instance

The foundational property everything else depends on. nginx round-robins
REST calls across `server1`/`server2` — a create, a join, and each player's
own resolve call can each independently land on a *different* backend, and
the system still has to converge both players onto the *same* instance for
actual gameplay.

```bash
# 1. Create the game (round-robins to server1 or server2 — doesn't matter which)
curl -s -X POST http://localhost:8080/games \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$(uuidgen)\"}" | tee /tmp/create.json | jq .

GAME_ID=$(jq -r '.data.gameID' /tmp/create.json)
WHITE_TOKEN=$(jq -r '.data.playerToken' /tmp/create.json)

# 2. Join as Black (a separate round-robin roll of the dice)
curl -s -X POST http://localhost:8080/games/$GAME_ID/join \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$(uuidgen)\"}" | tee /tmp/join.json | jq .

BLACK_TOKEN=$(jq -r '.data.playerToken' /tmp/join.json)

# 3. Resolve BOTH players — each call round-robins independently
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | tee /tmp/resolve_white.json | jq .
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $BLACK_TOKEN" | tee /tmp/resolve_black.json | jq .
```

**Watch for:** `.data.instanceLabel` in both `resolve_white.json` and
`resolve_black.json` — **they must be identical**, regardless of which
backend actually answered each REST call. That's the whole point: the
resolve endpoint's job is convergence, not the REST round-robin itself. If
they differ, something is broken before you go any further — stop and check
`docker compose logs server1 server2` for `ClaimOwnership`/`GetOwner`
errors.

```bash
WHITE_CONNECT=$(jq -r '.data.connectToken' /tmp/resolve_white.json)
WHITE_WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_white.json)
BLACK_CONNECT=$(jq -r '.data.connectToken' /tmp/resolve_black.json)
BLACK_WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_black.json)

echo "White dials: ws://localhost:8080${WHITE_WS_PATH}?token=$WHITE_CONNECT"
echo "Black dials: ws://localhost:8080${BLACK_WS_PATH}?token=$BLACK_CONNECT"
```

connectTokens expire in ~10s (`ConnectClaimsTTL`) — dial immediately. Open
two terminals:

```bash
# Terminal 1 (White)
npx wscat -c "ws://localhost:8080${WHITE_WS_PATH}?token=$WHITE_CONNECT"

# Terminal 2 (Black)
npx wscat -c "ws://localhost:8080${BLACK_WS_PATH}?token=$BLACK_CONNECT"
```

**Watch for:** both connect successfully, White sees `GAME_STATE`
(`WAITING_FOR_PLAYER`, then `OPPONENT_CONNECTED` once Black connects), Black
sees `GAME_STATE` (`ACTIVE`). In White's terminal, send a move:

```json
{"type":"MOVE","san":"e4"}
```

**Watch for:** both terminals receive `MOVE_APPLIED`. This confirms both
connections are genuinely on the *same* live `GameSession` — real
co-location, not just matching labels on paper. Leave both connections open
and move onto Scenario 1.

---

## Scenario 1: Failover — kill the owning instance mid-game, reconnect elsewhere

Continuing from Scenario 0 (a live game, one move played, both players
still connected).

```bash
# Which instance actually owns this game?
docker compose exec redis redis-cli GET game:$GAME_ID
# -> "server1" or "server2" — call this $OWNER below
```

```bash
docker compose stop $OWNER
```

**Watch for:** the *other* container's logs stay quiet (it never owned this
game) — nothing should happen yet. The player connections to `$OWNER` are
now dead at the TCP level; your wscat terminal for whichever player was
connected there will eventually show a disconnect (gorilla's client doesn't
always surface this instantly — Ctrl+C and reconnect is fine).

Have **both** players re-resolve and reconnect:

```bash
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | tee /tmp/resolve_white2.json | jq .
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $BLACK_TOKEN" | tee /tmp/resolve_black2.json | jq .
```

**Watch for:**
- `.data.instanceLabel` in both responses is now the **surviving** instance
  (not `$OWNER`) — confirm via `redis-cli GET game:$GAME_ID` too.
- This should happen **fast** — within a few seconds, not waiting out the
  full 30s ownership TTL. That's the whole point of the two-key
  ownership/liveness split (ADR-023): the liveness key (10s TTL) expires
  well before ownership's 30s, so the survivor detects "confirmed dead" and
  takes over immediately rather than waiting.
- Check `docker compose logs <survivor>` for a `"game hydrated on resolve"`
  log line — proof the survivor actually rebuilt the session from Postgres,
  not just claimed ownership without any real state.

Dial both players against the new `wsPath`/`connectToken` values, confirm
`GAME_STATE` shows **the move already played** (`e4`, White to move again is
wrong — Black to move, since White already moved) — this is the real
correctness check: the board wasn't lost, just relocated.

---

## Scenario 2: Recovered instance must NOT re-acquire — the split-brain test

Directly continuing from Scenario 1. `$OWNER` is stopped; the game has
already failed over to the survivor and at least one more move has ideally
been played post-failover (play one more move now if you haven't, so there's
a real difference between "state before failover" and "state after" to
check).

```bash
docker compose start $OWNER
# give it a few seconds to finish booting (health check will tell you)
docker compose ps $OWNER
```

**Watch for:** `docker compose logs $OWNER` on restart — it should **not**
log anything claiming or hydrating `$GAME_ID`. It has no reason to; nothing
told it this game exists in its (empty, fresh-process) local registry, and
it has no route that would make it proactively re-scan for old ownership
(this is exactly ADR-024's dropped-eager-restore decision paying off).

```bash
docker compose exec redis redis-cli GET game:$GAME_ID
```

**Watch for:** still the survivor, **not** `$OWNER`. This is the actual
regression test — the original ID-prefix and consistent-hash-ring designs
(rejected in ADR-021) both failed exactly here: a recovered instance would
reconstruct its own independent copy of the game. Confirm one more time by
resolving again:

```bash
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | jq .data.instanceLabel
```

Should still print the survivor. If this ever prints `$OWNER` instead,
that's a real split-brain bug — stop and investigate before continuing.

---

## Scenario 3: One player never reconnects — normal abandonment

Start a **fresh** game for this one (Scenario 1/2's game already has
non-standard history that'll make outcome-reading confusing). Repeat
Scenario 0 up through both players connected and active.

```bash
docker compose exec redis redis-cli GET game:$GAME_ID   # note the owner
docker compose stop <owner>
```

This time, reconnect **only one** player (say, White) via resolve +
wscat. Deliberately do **not** reconnect Black.

**Watch for:** nothing dramatic for the first ~60 seconds — this is a real
wall-clock wait, not something to rush; the abandonment timer is a genuine
60s timer (`abandonTimeout` in `manager.go`), same as Phase 1. Watch
White's wscat terminal.

After ~60s:

```bash
curl -s http://localhost:8080/games/$GAME_ID | jq .
```

**Watch for:** `status: "COMPLETED"`, `outcome: "WHITE"` (White wins by
Black's abandonment), `outcomeReason: "ABANDONED"` — the single-player
abandonment path (ADR-015), unmodified by any of Phase 2's routing work, per
PHASE_2.md's explicit claim that this scenario is "indistinguishable from an
ordinary Phase 1 single-player disconnect from the new instance's
perspective."

---

## Scenario 4: Redis goes down mid-game

Fresh game again, both players connected and actively playing (Scenario 0
through the move exchange).

```bash
docker compose stop redis
```

**Watch for:** the existing wscat connections **do not drop**. Send another
move from whichever player's turn it is:

```json
{"type":"MOVE","san":"e5"}
```

**Watch for:** `MOVE_APPLIED` arrives normally in both terminals. Ordinary
gameplay never touches Redis — only resolve calls do — so this should be
completely unaffected. This is the direct proof of PHASE_2.md's "Known
Limitations" claim: "Redis being down blocks new routing decisions, not live
gameplay."

Now try a **new** resolve call — either a reconnect attempt for this same
game, or start an entirely new game and try to resolve it:

```bash
curl -s -w "\nHTTP %{http_code}\n" http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN"
```

**Watch for:** a clean `500` (or similar), **not** a hung connection and
**not** a crashed container. Confirm the container is still up:

```bash
docker compose ps server1 server2
```

Both should still show `healthy`/`Up` — a Redis outage must never crash an
instance, only degrade its ability to answer *new* resolve calls. Check the
logs for a clearly-logged error (not a panic stack trace):

```bash
docker compose logs --tail 20 server1 server2
```

Restore Redis and confirm recovery:

```bash
docker compose start redis
# wait for its healthcheck
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | jq .
```

**Watch for:** resolve succeeds again once Redis is healthy — no manual
intervention needed on the server side, no stuck state.

---

## Scenario 5: Reconnect to an already-completed game

```bash
curl -s -X POST http://localhost:8080/games \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$(uuidgen)\"}" | tee /tmp/create.json | jq .
GAME_ID=$(jq -r '.data.gameID' /tmp/create.json)
WHITE_TOKEN=$(jq -r '.data.playerToken' /tmp/create.json)

curl -s -X POST http://localhost:8080/games/$GAME_ID/join \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$(uuidgen)\"}" | tee /tmp/join.json | jq .
BLACK_TOKEN=$(jq -r '.data.playerToken' /tmp/join.json)
```

Resolve and connect both players as in Scenario 0, then have White resign:

```json
{"type":"RESIGN"}
```

**Watch for:** both players receive `GAME_OVER`, then both connections
close normally (Phase 1 behavior, unaffected by Phase 2). Confirm terminal
state:

```bash
curl -s http://localhost:8080/games/$GAME_ID | jq .
# status: COMPLETED, outcome: BLACK, outcomeReason: RESIGNATION
```

Now — the actual Step 5/11 regression test — resolve **again**, well after
the game ended:

```bash
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $BLACK_TOKEN" | tee /tmp/resolve_after.json | jq .
```

**Watch for:** `200`, a valid `connectToken`/`wsPath` — **not** an error.
This is the case Phase 1 never had to handle at all (no session ever existed
for reconnecting to a finished game before Phase 2's hydrate-on-miss path).

```bash
CONNECT=$(jq -r '.data.connectToken' /tmp/resolve_after.json)
WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_after.json)
npx wscat -c "ws://localhost:8080${WS_PATH}?token=$CONNECT"
```

**Watch for:** `GAME_STATE` delivered immediately, `status: "COMPLETED"`,
`outcome: "BLACK"`, `outcomeReason: "RESIGNATION"` — the correct terminal
state, not an error, not a hang.

**Also worth checking** (this is TD-P2-004, flagged during Step 5 — not a
bug, but confirm you understand the tradeoff): check which instance now
owns this terminal game in Redis, and note that this session will sit in
that instance's registry indefinitely — nothing currently evicts it. Not
something to "fix" here, just something to see for yourself:

```bash
docker compose exec redis redis-cli GET game:$GAME_ID
```

---

## Scenario 6: Opponent never joins, creator disconnects — ABORTED (`DECISIONS_LOG_PHASE_2.md` ADR-029)

Distinct from every scenario above: this one never reaches `ACTIVE` at all.
A creator connects, nobody ever joins as the opponent, and the creator then
disconnects. Before ADR-029, this got permanently stuck in
`WAITING_FOR_PLAYER` (TD-P2-005) — the state machine had no edge out of it
at all. The correct terminal outcome is `ABORTED`: no winner, no outcome,
void — distinct from `ABANDONED`, which requires the game to have actually
reached `ACTIVE` (both players joined) first.

```bash
curl -s -X POST http://localhost:8080/games \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$(uuidgen)\"}" | tee /tmp/create.json | jq .
GAME_ID=$(jq -r '.data.gameID' /tmp/create.json)
WHITE_TOKEN=$(jq -r '.data.playerToken' /tmp/create.json)
```

Deliberately do **not** join as Black at all for this game.

```bash
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | tee /tmp/resolve_white.json | jq .
CONNECT=$(jq -r '.data.connectToken' /tmp/resolve_white.json)
WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_white.json)
npx wscat -c "ws://localhost:8080${WS_PATH}?token=$CONNECT"
```

**Watch for:** `GAME_STATE` with `status: "WAITING_FOR_PLAYER"` — correct,
nobody has joined yet.

Disconnect White (Ctrl+C the wscat session) — simulating the creator giving
up and leaving without ever getting an opponent.

**Watch for:** nothing dramatic for ~60 seconds — same real wall-clock wait
as Scenario 3, same underlying timer (`abandonTimeout` in `manager.go`),
just a different terminal outcome because this game never reached `ACTIVE`.

After ~60s:

```bash
curl -s http://localhost:8080/games/$GAME_ID | jq .
```

**Watch for:** `status: "ABORTED"`, `outcome: null`, `outcomeReason: null`.
Not `COMPLETED`, not `ABANDONED` — both of those imply the game actually
started. `ABORTED` is a distinct terminal state (ADR-029) specifically for
"this game never began." If you instead see the game still stuck at
`WAITING_FOR_PLAYER` after 60+ seconds, that's TD-P2-005's original bug —
stop and check that migration 004 actually ran (`docker compose logs
chess_migrate`) before investigating further.

**Shortcut:** `./e2e-phase2.sh scenario6` automates this whole sequence
(connect, disconnect, the 65s wait, and the final status check) if you'd
rather not do it by hand.

---

## Wrap-up

```bash
docker compose down   # stop everything; add -v to also wipe volumes
```

For each scenario, note in `CLAUDE.md`'s session log (or wherever you're
tracking this) whether it passed as described. If any scenario diverges from
"Watch for," that's a real bug to bring back for review — not something to
paper over or re-run hoping for a different result, per this project's own
"documentation is never source of truth, correctness over documentation
preservation" principle: if reality disagrees with what PHASE_2.md/the ADRs
predicted, the design docs get corrected to match what's actually true, not
the other way around.
