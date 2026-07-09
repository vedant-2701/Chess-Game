# PHASE 1 — Step 14: End-to-End Verification Walkthrough

Maps directly to PHASE_1.md's Step 14 checklist and Acceptance Criteria table.
No frontend — a browser UI is Phase 7 scope (ROADMAP.md). This uses `curl` for
REST and `wscat` for WebSocket, in separate terminals, against the real
running binary. This is also the **first real execution** of `runMigrations`
and the full graceful-shutdown sequence — neither has run outside `go build`
until now.

You'll want **4 terminals**:
- **Terminal 1** — the server process
- **Terminal 2** — REST "setup" calls (create game, join, inspect DB)
- **Terminal 3** — White's WebSocket session (`wscat`)
- **Terminal 4** — Black's WebSocket session (`wscat`)

---

## 0. One-time setup

```bash
cd ~/chess-server

# .env must exist — copy from the template if you haven't already.
[ -f .env ] || cp .env.example .env

# jq makes reading JSON responses and extracting tokens much less painful.
sudo apt-get update && sudo apt-get install -y jq

# wscat needs Node/npm. If you don't have Node at all:
#   curl -fsSL https://deb.nodesource.com/setup_lts.x | sudo -E bash -
#   sudo apt-get install -y nodejs
npm install -g wscat

# Fallback if npm/Node is a pain to set up: websocat (single static binary)
#   curl -L https://github.com/vi/websocat/releases/latest/download/websocat.x86_64-unknown-linux-musl -o /usr/local/bin/websocat
#   chmod +x /usr/local/bin/websocat
# (commands below use wscat syntax; websocat's is `websocat "ws://..."`, same idea)
```

PostgreSQL is already running per `make docker-up`, confirmed.

---

## 1. Start the server (Terminal 1)

Build a real binary rather than `go run` — `go run` spawns a wrapper process,
which makes `kill -9` in the restart test (§6 below) target the wrong PID.

```bash
make build
./bin/server
```

**Watch for, in order:**
1. Migration logs — this is `runMigrations`'s first real execution. If the
   `pgx5://` scheme is wrong despite the source-level verification, it fails
   right here, loudly, before anything else starts.
2. `"server starting" port=8080`

If it doesn't get past migrations, stop and paste the error — don't proceed
to the rest of this checklist with an unverified migration step.

**Sanity check, Terminal 2:**
```bash
curl -s http://localhost:8080/health
```
Expect exactly `{"status":"ok"}` — flat, no `"data"` wrapper. This is the
`/health` envelope decision made earlier in this session, confirmed live.

---

## 2. Create and join a game (Terminal 2)

```bash
WHITE_USER_ID=$(uuidgen)
curl -s -X POST http://localhost:8080/games \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$WHITE_USER_ID\"}" | tee /tmp/create.json | jq .

GAME_ID=$(jq -r '.data.gameID' /tmp/create.json)
WHITE_TOKEN=$(jq -r '.data.playerToken' /tmp/create.json)

BLACK_USER_ID=$(uuidgen)
curl -s -X POST http://localhost:8080/games/$GAME_ID/join \
  -H "Content-Type: application/json" \
  -d "{\"userID\":\"$BLACK_USER_ID\"}" | tee /tmp/join.json | jq .

BLACK_TOKEN=$(jq -r '.data.playerToken' /tmp/join.json)

# Confirm state before either WS connects — should be WAITING_FOR_PLAYER.
curl -s http://localhost:8080/games/$GAME_ID | jq .

echo "GAME_ID=$GAME_ID"
echo "WHITE_TOKEN=$WHITE_TOKEN"
echo "BLACK_TOKEN=$BLACK_TOKEN"
```

Copy the three printed values — you'll paste them as literal strings into
Terminals 3 and 4 (shell variables don't cross terminal sessions).

---

## 3. Connect both players

**Terminal 3 (White):**
```bash
wscat -c "ws://localhost:8080/ws/game/<GAME_ID>?token=<WHITE_TOKEN>"
```

**Terminal 4 (Black):**
```bash
wscat -c "ws://localhost:8080/ws/game/<GAME_ID>?token=<BLACK_TOKEN>"
```

**Expect:**
- Both terminals receive `GAME_STATE` with `status: "ACTIVE"` (the second
  connection is what flips WAITING → ACTIVE — this exercises ADR-017's
  atomic `RegisterConnection` fix).
- Both receive `OPPONENT_CONNECTED`.
- `GET /games/<GAME_ID>` (Terminal 2) now shows `"status":"ACTIVE"`.

---

## 4. Play moves — AC #1 and AC #10 (turn enforcement)

In **Terminal 3 (White)**, send:
```json
{"type":"MOVE","san":"e4"}
```
Both terminals should receive `MOVE_APPLIED`.

**AC #10 — wrong-turn rejection:** immediately send another move from
**White again** (before Black moves):
```json
{"type":"MOVE","san":"Nf3"}
```
Expect `MOVE_REJECTED` with `reason: "not your turn"`, sent only to White.

In **Terminal 4 (Black)**, send:
```json
{"type":"MOVE","san":"e5"}
```

**AC #4 — illegal move rejection:** from whoever's turn it is, send a
structurally-illegal move:
```json
{"type":"MOVE","san":"Qh5xz"}
```
Expect `MOVE_REJECTED` with `reason: "illegal move"`, and confirm via
`GET /games/<GAME_ID>` that `currentFEN` did **not** change.

---

## 5. Reconnection — AC #2

In Terminal 3, disconnect White (`Ctrl+C` or `Ctrl+D`).

**Expect:** Terminal 4 (Black) receives `OPPONENT_DISCONNECTED`.

Reconnect White with the **same token**:
```bash
wscat -c "ws://localhost:8080/ws/game/<GAME_ID>?token=<WHITE_TOKEN>"
```

**Expect:**
- White receives `GAME_STATE` reflecting the exact current board/move list.
- Black receives `OPPONENT_RECONNECTED`.

---

## 6. Server restart mid-game — AC #3 (the big one)

This is the first real test of `RestoreActiveGames`, and — since you'll
disconnect a player first — this session's `HandleDisconnect` clock-persist
fix.

1. In Terminal 3, **disconnect White** but do **not** reconnect yet.
2. Wait ~5–10 seconds (so some real time elapses since White's last move —
   this is what proves the clock-persist fix, not just that a value exists).
3. Inspect the DB directly, Terminal 2:
   ```bash
   docker compose exec postgres psql -U chess -d chess \
     -c "SELECT id, status, white_time_ms, black_time_ms FROM games WHERE id = '$GAME_ID';"
   ```
   Note the `white_time_ms` value — it should already reflect the pause
   (not the pre-disconnect value), confirming the fix persisted on
   disconnect, not just in memory.
4. Find and kill the server **hard** — `kill -9`, not Ctrl+C (that's the
   graceful path, tested separately in §7):
   ```bash
   pgrep -f ./bin/server
   kill -9 <PID>
   ```
5. Restart:
   ```bash
   ./bin/server
   ```
   Watch the logs for `RestoreActiveGames` activity.
6. Reconnect **both** players with their original tokens.

**Expect:** both receive `GAME_STATE` with the correct board (all moves
from §4 intact), correct `turn`, and clock values consistent with what was
in the DB (not reset to 600000ms).

---

## 7. Graceful shutdown — validates the ADR-018 + Step 13 shutdown sequence

With both players still connected and mid-game:

1. In Terminal 1, send `Ctrl+C` (SIGINT).
2. Watch the log sequence — should show, **in order**: HTTP server shutdown,
   context cancellation, WebSocket registry `CloseAll`, `PersistActiveClockState`,
   pool close, `"shutdown sequence complete"`.
3. Both wscat terminals should receive a close frame, not just hang.
4. Terminal 2: `curl http://localhost:8080/health` should now fail with
   connection refused.

---

## 8. Resignation — AC (GAME_OVER / RESIGNATION)

Start a fresh game (repeat §2–3), then from either player's terminal:
```json
{"type":"RESIGN"}
```
Expect `GAME_OVER` broadcast to both, `reason: "RESIGNATION"`,
`outcome` set to the opponent's color. Confirm via `GET /games/<GAME_ID>`
that `status` is `"COMPLETED"`.

---

## 9. Timeout — AC #5 (optional, impractical at real 10-minute length)

Not realistically testable end-to-end without either waiting the full 10
minutes once, or temporarily editing `InitialTimeMs` in `session.go` down to
something like `10_000` (10 seconds) for a single manual run, then reverting
the change before committing anything. Your call whether this is worth doing
now or deferred — it's lower-risk than §6 since the timeout path itself
isn't something this session touched.

---

## Acceptance Criteria Checklist (PHASE_1.md)

| # | Criterion | Covered by |
|---|-----------|-----------|
| 1 | Full game start-to-checkmate on different machines | §4 (partial — full checkmate optional, mechanics proven) |
| 2 | Close/reopen browser mid-game resumes state | §5 |
| 3 | Kill + restart mid-game resumes correctly | §6 |
| 4 | Illegal move rejected, board unchanged | §4 |
| 5 | Clock-zero timeout detected server-side | §9 (optional) |
| 6 | `go test -race ./...` passes | already confirmed this session |
| 7 | No goroutine leaks | `session.clock.Stop()` / `t.Cleanup` patterns already in place; watch server logs during §7 for any hang |
| 8 | `migrate-down && migrate-up` reversible | not covered above — run separately: `make migrate-down && make migrate-up` |
| 9 | Errors logged with gameID + context | eyeball Terminal 1's logs during §4 and §6 |
| 10 | Wrong-turn move rejected | §4 |

Report back what passes and what doesn't — especially §1 (migrations) and
§6 (the clock-persist fix under a real hard kill), since those are the two
things this session touched that have never actually run before now.
