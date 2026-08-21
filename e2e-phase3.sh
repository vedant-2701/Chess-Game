#!/usr/bin/env bash
# e2e-phase3.sh — Phase 3 matchmaking E2E helper for chess-server.
#
# Companion script to e2e-walkthrough/phase3_step7_e2e_walkthrough.md, same
# spirit and conventions as e2e-phase2.sh: manual, real-process testing
# against the actual cluster (including matchmaking-service now), not
# unit/integration tests. State persists across invocations in
# /tmp/chess-e2e-p3/state.env, same pattern as e2e-phase2.sh, in a SEPARATE
# state directory so the two scripts never collide if run in the same
# session.
#
# IMPORTANT — this used to document a real gap (TD-P3-007) and work around
# it with a throwaway POST /games call. That gap is now fixed properly:
# POST /users (internal/api/auth_handler.go) creates a real users row
# directly, independent of game creation, specifically so
# POST /matchmaking/queue callers (which matchmaking-service itself can
# never seed, since it never touches Postgres — ARCHITECTURE.md's
# Matchmaking section) have a real row to satisfy CreateMatchedGame's FK
# constraint on users. mm_seed_user below calls that endpoint now, not a
# throwaway POST /games. Kept as a named function, not inlined, so
# scenario5 can still deliberately skip it to exploit the exact same
# FK-violation path on purpose (see scenario5's own comments).
#
# SSE streams (GET /matchmaking/stream) are one-directional (server->client
# only), unlike the WebSocket connections e2e-phase2.sh manages — no FIFO is
# needed, just a backgrounded `curl -N` writing to a log file. Matched-game
# WebSocket connections (after MATCH_FOUND) reuse e2e-phase2.sh's exact
# FIFO-based pattern, since that side of the flow is genuinely bidirectional
# (PHASE_3.md: "after receiving MATCH_FOUND, the client's connect flow is
# identical to shared-link ordinary reconnect").
#
# Requires: curl, jq, uuidgen, docker compose, npx (for wscat, matched-game
# connections only) — same as e2e-phase2.sh.

set -euo pipefail

BASE_URL="${CHESS_BASE_URL:-http://localhost:8080}"
STATE_DIR="${CHESS_E2E_STATE_DIR:-/tmp/chess-e2e-p3}"
STATE_FILE="$STATE_DIR/state.env"
LOG_DIR="./logs"
WS_READY_TIMEOUT="${CHESS_E2E_WS_READY_TIMEOUT:-20}"   # seconds; npx cold start can be slow
SSE_WAIT_TIMEOUT="${CHESS_E2E_SSE_WAIT_TIMEOUT:-15}"   # seconds; pairing-loop tick + gRPC report round trip
mkdir -p "$STATE_DIR"

# --- state helpers (identical to e2e-phase2.sh) -------------------------

load_state() {
  # shellcheck disable=SC1090
  [ -f "$STATE_FILE" ] && source "$STATE_FILE"
  return 0
}

set_var() {
  local key="$1" val="$2"
  touch "$STATE_FILE"
  grep -v "^${key}=" "$STATE_FILE" > "$STATE_FILE.tmp" 2>/dev/null || true
  mv "$STATE_FILE.tmp" "$STATE_FILE"
  printf '%s=%q\n' "$key" "$val" >> "$STATE_FILE"
}

unset_var() {
  local key="$1"
  [ -f "$STATE_FILE" ] || return 0
  grep -v "^${key}=" "$STATE_FILE" > "$STATE_FILE.tmp" 2>/dev/null || true
  mv "$STATE_FILE.tmp" "$STATE_FILE"
}

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 1; }
}

pause() { read -r -p "Press Enter to continue... " _ || true; }

# --- user seeding (via POST /users — fixes TD-P3-007) ------------------

mm_seed_user() {
  require curl; require jq; require uuidgen
  local uname resp ok uid
  # username must satisfy AuthHandler's 3-32 char bound; a uuidgen-derived
  # string comfortably does, and is unique enough not to collide in practice
  # across a single test run.
  uname="e2e_$(uuidgen | tr -d '-' | cut -c1-20)"
  resp=$(curl -s -X POST "$BASE_URL/users" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"$uname\",\"password\":\"e2e-test-password-not-secret\"}")
  uid=$(echo "$resp" | jq -r '.data.userID // empty')
  if [ -z "$uid" ]; then
    echo "mm_seed_user: POST /users failed for $uname" >&2
    echo "$resp" >&2
    return 1
  fi
  echo "$uid"
}

# --- queue lifecycle ------------------------------------------------------

# mm_queue <label> [seed=yes|no]
# seed=no deliberately skips mm_seed_user — see this file's header and
# scenario5.
mm_queue() {
  local label="$1" seed="${2:-yes}"
  require curl; require jq
  local uid
  if [ "$seed" = "yes" ]; then
    uid=$(mm_seed_user) || return 1
  else
    require uuidgen
    uid=$(uuidgen)
  fi
  local resp status body token
  resp=$(curl -s -w '\n%{http_code}' -X POST "$BASE_URL/matchmaking/queue" \
    -H "Content-Type: application/json" -d "{\"userID\":\"$uid\"}")
  status=$(echo "$resp" | tail -n1)
  body=$(echo "$resp" | sed '$d')
  echo "$body" | jq . 2>/dev/null || echo "$body"
  echo "HTTP $status"
  set_var "${label^^}_USER_ID" "$uid"
  token=$(echo "$body" | jq -r '.data.matchmakingToken // empty')
  if [ -n "$token" ]; then
    set_var "${label^^}_MM_TOKEN" "$token"
    echo "$label queued (userID=$uid)"
  else
    echo "$label: queue call returned no token (HTTP $status) — see body above" >&2
  fi
}

mm_status() {
  local label="$1"
  load_state
  local token_var="${label^^}_MM_TOKEN"
  local token="${!token_var:-}"
  [ -z "$token" ] && { echo "no matchmaking token for $label" >&2; return 1; }
  curl -s "$BASE_URL/matchmaking/status?token=$token" | jq .
}

mm_cancel() {
  local label="$1"
  load_state
  local token_var="${label^^}_MM_TOKEN"
  local token="${!token_var:-}"
  [ -z "$token" ] && { echo "no matchmaking token for $label" >&2; return 1; }
  curl -s -X DELETE "$BASE_URL/matchmaking/queue?token=$token" | jq .
}

# --- SSE stream (GET /matchmaking/stream) --------------------------------

sse_connect() {
  local label="$1"
  require curl
  load_state
  local token_var="${label^^}_MM_TOKEN"
  local token="${!token_var:-}"
  if [ -z "$token" ]; then
    echo "No matchmaking token for $label — run: $0 queue $label" >&2
    return 1
  fi
  local log="$STATE_DIR/${label}.sse.log"
  sse_disconnect "$label" >/dev/null 2>&1 || true
  : > "$log"
  # -N: disable curl's own output buffering — required to see SSE frames as
  # they arrive rather than only once the connection closes.
  curl -N -s "$BASE_URL/matchmaking/stream?token=$token" > "$log" 2>&1 &
  local pid=$!
  disown "$pid" 2>/dev/null || true
  set_var "${label^^}_SSE_PID" "$pid"
  set_var "${label^^}_SSE_LOG" "$log"
  echo "$label SSE stream connecting in background (pid $pid) -> $log"
}

# sse_wait_event <label> <MATCH_FOUND|MATCHMAKING_FAILED> [timeout]
# Polls the log for the frame's "event: TYPE" line — NOT a fixed sleep, same
# reasoning as e2e-phase2.sh's ws_wait_ready: the pairing-loop tick interval
# (MATCHMAKING_PAIRING_INTERVAL_MS, default 500ms) plus the gRPC report round
# trip is not tightly enough bounded for a guessed sleep to be reliable.
sse_wait_event() {
  local label="$1" event="$2" timeout="${3:-$SSE_WAIT_TIMEOUT}"
  load_state
  local log_var="${label^^}_SSE_LOG"
  local log="${!log_var:-}"
  if [ -z "$log" ]; then
    echo "no SSE log for $label — run: $0 sse-connect $label" >&2
    return 1
  fi
  local start=$SECONDS
  while (( SECONDS - start < timeout )); do
    if grep -q "^event: $event\$" "$log" 2>/dev/null; then
      echo "$label: received event '$event' ($(( SECONDS - start ))s)."
      return 0
    fi
    sleep 0.3
  done
  echo "WARNING: $label did not receive event '$event' within ${timeout}s." >&2
  echo "         Check the raw log: $0 sse-tail $label" >&2
  return 1
}

# sse_event_data <label> <event> — prints the JSON payload from the frame's
# "data: " line immediately following the matching "event: TYPE" line
# (writeSSEEvent's exact two-line-per-frame shape: "event: %s\ndata: %s\n\n").
sse_event_data() {
  local label="$1" event="$2"
  load_state
  local log_var="${label^^}_SSE_LOG"
  local log="${!log_var:-}"
  [ -z "$log" ] && return 1
  awk -v ev="event: $event" '
    $0 == ev { found=1; next }
    found && index($0, "data: ") == 1 { print substr($0, 7); exit }
  ' "$log"
}

sse_tail() {
  local label="$1"
  load_state
  local log_var="${label^^}_SSE_LOG"
  local log="${!log_var:-}"
  [ -z "$log" ] && { echo "no SSE log for $label yet" >&2; return 1; }
  tail -n 40 -f "$log"
}

sse_disconnect() {
  local label="$1"
  load_state
  local pid_var="${label^^}_SSE_PID"
  local pid="${!pid_var:-}"
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  unset_var "${label^^}_SSE_PID"
}

# --- WebSocket connections after MATCH_FOUND -----------------------------
# PHASE_3_DESIGN_NOTES.md §18 (2026-08-17): two distinct connect paths now
# exist, deliberately different from each other:
#   mm_ws_connect_from_match  — the initial "just matched" connection, using
#                               MATCH_FOUND's connectToken directly. No
#                               /resolve round-trip — CreateMatchedGame
#                               minted a genuinely dialable ConnectClaims
#                               right there, synchronously, with zero
#                               staleness window.
#   mm_ws_reconnect_via_resolve — a LATER reconnect (after a disconnect, a
#                               long gap, or simply demonstrating the
#                               fallback path), using the playerToken
#                               captured by mm_ws_connect_from_match and
#                               going through GET /games/{id}/resolve —
#                               identical to an ordinary shared-link
#                               reconnect, because it IS that same path,
#                               reused.

# _ws_dial <label> <url> — shared FIFO-based background wscat connection
# machinery for both paths above; only the URL differs between them.
_ws_dial() {
  local label="$1" url="$2"
  require npx
  set_var "${label^^}_WS_URL" "$url"

  local fifo="$STATE_DIR/${label}.fifo"
  local log="$STATE_DIR/${label}.ws.log"
  ws_disconnect "$label" >/dev/null 2>&1 || true
  rm -f "$fifo"
  mkfifo "$fifo"
  : > "$log"

  (
    exec 3<>"$fifo"
    wscat -c "$url" <&3 > "$log" 2>&1
  ) &
  local pid=$!
  disown "$pid" 2>/dev/null || true

  set_var "${label^^}_WS_PID" "$pid"
  set_var "${label^^}_WS_FIFO" "$fifo"
  set_var "${label^^}_WS_LOG" "$log"
}

mm_ws_connect_from_match() {
  local label="$1"
  require jq
  load_state
  local data
  data=$(sse_event_data "$label" MATCH_FOUND)
  if [ -z "$data" ]; then
    echo "no MATCH_FOUND data captured for $label yet — run: $0 sse-wait $label MATCH_FOUND" >&2
    return 1
  fi

  local connect wspath gid player_token
  connect=$(echo "$data" | jq -r '.connectToken')
  wspath=$(echo "$data" | jq -r '.wsPath')
  gid=$(echo "$data" | jq -r '.gameID')
  player_token=$(echo "$data" | jq -r '.playerToken')
  set_var "${label^^}_GAME_ID" "$gid"
  # Captured for mm_ws_reconnect_via_resolve's later use — not needed for
  # this connection itself.
  set_var "${label^^}_PLAYER_TOKEN" "$player_token"

  local url="${BASE_URL/http/ws}${wspath}?token=${connect}"
  _ws_dial "$label" "$url"
  load_state
  local pid_var="${label^^}_WS_PID"
  echo "$label connecting directly to matched game $gid — no /resolve needed (pid ${!pid_var})..."
}

# mm_ws_reconnect_via_resolve <label>
# Demonstrates the OTHER path: a reconnect using the playerToken captured by
# a prior mm_ws_connect_from_match call, going through
# GET /games/{id}/resolve exactly like an ordinary shared-link reconnect.
# Typical use: wsdisconnect <label>, then this, to prove the fallback path
# independently still works — not just that the fast path does.
mm_ws_reconnect_via_resolve() {
  local label="$1"
  require curl; require jq
  load_state
  local gid_var="${label^^}_GAME_ID"
  local token_var="${label^^}_PLAYER_TOKEN"
  local gid="${!gid_var:-}"
  local player_token="${!token_var:-}"
  if [ -z "$gid" ] || [ -z "$player_token" ]; then
    echo "no captured gameID/playerToken for $label — run: $0 mwconnect $label first" >&2
    return 1
  fi

  local resolve_resp connect wspath
  resolve_resp=$(curl -s "$BASE_URL/games/$gid/resolve" -H "Authorization: Bearer $player_token")
  connect=$(echo "$resolve_resp" | jq -r '.data.connectToken // empty')
  wspath=$(echo "$resolve_resp" | jq -r '.data.wsPath // empty')
  if [ -z "$connect" ]; then
    echo "$label: GET /games/$gid/resolve failed" >&2
    echo "$resolve_resp" >&2
    return 1
  fi

  local url="${BASE_URL/http/ws}${wspath}?token=${connect}"
  _ws_dial "$label" "$url"
  echo "$label reconnected via /resolve to game $gid..."
}

ws_wait_ready() {
  local label="$1" timeout="${2:-$WS_READY_TIMEOUT}"
  load_state
  local log_var="${label^^}_WS_LOG"
  local log="${!log_var:-}"
  if [ -z "$log" ]; then
    echo "no log for $label — run: $0 mwconnect $label" >&2
    return 1
  fi
  local start=$SECONDS
  while (( SECONDS - start < timeout )); do
    if grep -qi -E "Connected|GAME_STATE" "$log" 2>/dev/null; then
      echo "$label: connection confirmed ready ($(( SECONDS - start ))s)."
      return 0
    fi
    sleep 0.3
  done
  echo "WARNING: $label's WebSocket did not report 'Connected' within ${timeout}s." >&2
  echo "         Check the raw log: $0 wstail $label" >&2
  return 1
}

ws_send() {
  local label="$1" msg="$2"
  load_state
  local fifo_var="${label^^}_WS_FIFO"
  local fifo="${!fifo_var:-}"
  if [ -z "$fifo" ] || [ ! -p "$fifo" ]; then
    echo "No active connection for $label — run: $0 mwconnect $label" >&2
    return 1
  fi
  echo "$msg" > "$fifo"
}

move() { ws_send "$1" "{\"type\":\"MOVE\",\"san\":\"$2\"}"; }
resign() { ws_send "$1" '{"type":"RESIGN"}'; }

ws_tail() {
  local label="$1"
  load_state
  local log_var="${label^^}_WS_LOG"
  local log="${!log_var:-}"
  [ -z "$log" ] && { echo "no log for $label yet" >&2; return 1; }
  tail -n 40 -f "$log"
}

ws_disconnect() {
  local label="$1"
  load_state
  local pid_var="${label^^}_WS_PID"
  local pid="${!pid_var:-}"
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  unset_var "${label^^}_WS_PID"
}

# --- introspection ---------------------------------------------------------

queue_card() {
  docker compose exec -T redis redis-cli ZCARD 'matchmaking:queue:10+0'
}

queue_members() {
  docker compose exec -T redis redis-cli ZRANGE 'matchmaking:queue:10+0' 0 -1 WITHSCORES
}

mm_redis_keys() {
  echo "--- matchmaking:queue:10+0 ---"
  queue_members
  echo "--- matchmaking_active_game:* ---"
  docker compose exec -T redis redis-cli KEYS 'matchmaking_active_game:*'
  echo "--- matchmaking_result:* ---"
  docker compose exec -T redis redis-cli KEYS 'matchmaking_result:*'
  echo "--- reported:* (ReportMatchCreated dedup keys) ---"
  docker compose exec -T redis redis-cli KEYS 'reported:*'
}

mm_state() {
  load_state
  env | grep -E '^(P[0-9A-Z_]*|R[0-9]+[AB]_)' | sort || true
}

matched_games_count() {
  docker compose exec -T postgres psql -U chess -d chess -tAc \
    "SELECT COUNT(*) FROM games WHERE matchmaking_request_id IS NOT NULL"
}

# --- cluster control ------------------------------------------------------

kill_instance() { docker compose stop "$1"; }
start_instance() { docker compose start "$1"; }
kill_mm() { docker compose stop matchmaking-service; }
start_mm() { docker compose start matchmaking-service; }

# --- guided scenarios (mirrors PHASE_3.md Step 7's checklist, in order) --

scenario1() {
  echo "=== Scenario 1: Two players queue, matched, connect directly, then p1 reconnects via /resolve ==="
  mm_queue p1
  mm_queue p2
  sse_connect p1
  sse_connect p2
  echo "Waiting for MATCH_FOUND on both SSE streams..."
  sse_wait_event p1 MATCH_FOUND
  sse_wait_event p2 MATCH_FOUND
  echo "--- p1 MATCH_FOUND data ---"; sse_event_data p1 MATCH_FOUND
  echo "--- p2 MATCH_FOUND data ---"; sse_event_data p2 MATCH_FOUND
  echo "Watch for: identical gameID on both, connectToken/playerToken/instanceLabel/wsPath"
  echo "all present (PHASE_3_DESIGN_NOTES.md §18: two distinct tokens now, not one)."
  pause
  mm_ws_connect_from_match p1
  mm_ws_connect_from_match p2
  ws_wait_ready p1
  ws_wait_ready p2
  load_state
  echo "--- p1 ws log ---"; tail -n 10 "$P1_WS_LOG"
  echo "--- p2 ws log ---"; tail -n 10 "$P2_WS_LOG"
  echo "Watch for: GAME_STATE on both — status flips WAITING_FOR_PLAYER -> ACTIVE only"
  echo "once BOTH have connected (CreateMatchedGame starts WAITING_FOR_PLAYER, per"
  echo "TD-P3-004/ADR-041/042 — not ACTIVE immediately on match). Neither connection went"
  echo "through GET /games/:id/resolve — connectToken was directly dialable."
  echo "Sending e4 as p1..."
  move p1 e4
  sleep 1
  load_state
  echo "--- p2 log (tail) ---"; tail -n 5 "$P2_WS_LOG"
  echo "Watch for: MOVE_APPLIED delivered to p2 — confirms real co-location, not just"
  echo "matching instanceLabels on paper."
  pause
  echo "Now demonstrating the OTHER path (PHASE_3_DESIGN_NOTES.md §18): disconnect p1,"
  echo "then reconnect using playerToken via GET /games/:id/resolve — the fallback this"
  echo "whole redesign exists to keep working, exercised independently of the fast path."
  ws_disconnect p1
  sleep 1
  mm_ws_reconnect_via_resolve p1
  ws_wait_ready p1
  load_state
  echo "--- p1 ws log after reconnect ---"; tail -n 10 "$P1_WS_LOG"
  echo "Watch for: p1 reconnects cleanly and receives a current GAME_STATE (should show"
  echo "e4 already played) — confirms /resolve's fallback path is unaffected by any of"
  echo "this session's changes to the fast path."
}

scenario2() {
  echo "=== Scenario 2: Three players queue — first two matched, third waits ==="
  mm_queue p1
  mm_queue p2
  mm_queue p3
  sse_connect p1
  sse_connect p2
  sse_connect p3
  echo "Waiting for MATCH_FOUND on p1/p2 only..."
  sse_wait_event p1 MATCH_FOUND
  sse_wait_event p2 MATCH_FOUND
  echo "Watch for: p1 and p2's MATCH_FOUND share the same gameID (Challenge 2's"
  echo "'two LOWEST scores, earliest arrivals' rule — p3, queued last, must NOT be in it)."
  echo "p3's status right now (expect \"status\":\"waiting\"):"
  mm_status p3
  echo "Queue depth right now (expect 1 — just p3):"
  queue_card
}

scenario3a() {
  echo "=== Scenario 3a: Player queues then voluntarily cancels (DELETE) ==="
  mm_queue p1
  echo "Queue depth before cancel:"; queue_card
  mm_cancel p1
  echo "Queue depth after cancel (expect one less):"; queue_card
  echo "Status after cancel — expect still \"waiting\", NOT an error and NOT \"failed\":"
  echo "Cancel doesn't write a result record (Handler.Cancel's doc comment) — \"waiting\""
  echo "here just means 'no result exists,' which remains true after a clean cancel."
  mm_status p1
}

scenario3b() {
  echo "=== Scenario 3b: Player queues, never cancels, sweep times them out ==="
  echo "Real wall-clock wait: MATCHMAKING_QUEUE_TIMEOUT_SECONDS (default 30s) plus one"
  echo "sweep tick (5s, internal/mmsvc/sweep.go's fixed interval) — waiting ~40s."
  mm_queue p1
  sse_connect p1
  echo "Queue depth right after queueing (expect p1 present):"; queue_card
  sleep 40
  echo "Queue depth after the wait (expect p1 gone):"; queue_card
  echo "p1's SSE log — watch for 'event: MATCHMAKING_FAILED' with reason QUEUE_TIMEOUT:"
  sse_tail p1 &
  local tailpid=$!
  sleep 1
  kill "$tailpid" 2>/dev/null || true
  echo "p1's status endpoint (expect status=failed, reason=QUEUE_TIMEOUT):"
  mm_status p1
}

scenario4() {
  local n="${1:-10}"
  echo "=== Scenario 4: $n rapid pairs — server1's and server2's pairing loops racing ZPOPMIN ==="
  echo "This is PHASE_3.md's actual core learning objective under real E2E test — not"
  echo "just internal/matchmaking's own -race unit coverage (TestPairingLoop_ConcurrentTicks_NoDoubleMatch"
  echo "simulates two Managers in-process; this exercises the real cluster, two real"
  echo "containers, two real independent tickers, over the actual network)."
  local before after created
  before=$(matched_games_count)
  for i in $(seq 1 "$n"); do
    mm_queue "r${i}a" >/dev/null
    mm_queue "r${i}b" >/dev/null
  done
  echo "Queued $((n*2)) players ($n pairs). Waiting for both pairing loops to drain the queue..."
  local start=$SECONDS
  while (( SECONDS - start < 30 )); do
    [ "$(queue_card)" = "0" ] && break
    sleep 1
  done
  echo "Final queue depth (want 0):"; queue_card
  after=$(matched_games_count)
  created=$((after - before))
  echo "Matched games created: $created (want exactly $n — no double-match, no lost pair)"
  if [ "$created" -ne "$n" ]; then
    echo "MISMATCH — this is exactly the double-match race PHASE_3.md's Key Technical"
    echo "Challenges section describes (if created > n), or a pair silently lost to an"
    echo "uncaught failure (if created < n). Stop and investigate before continuing."
  else
    echo "OK: ZPOPMIN's atomicity held under real concurrent instances."
  fi
}

scenario5() {
  echo "=== Scenario 5: CreateMatchedGame insert failure (FK violation) -> re-enqueue + MATCHMAKING_FAILED ==="
  echo "Deliberately queueing WITHOUT mm_seed_user — see this file's header. These"
  echo "userIDs have no row in 'users' at all, so CreateMatchedGame's INSERT fails its"
  echo "FK constraint exactly like pairing_test.go's"
  echo "TestPairingLoop_Tick_CreateMatchedGameFails_ReenqueuesAndReportsFailure."
  mm_queue p1 no
  mm_queue p2 no
  sse_connect p1
  sse_connect p2
  echo "Waiting for MATCHMAKING_FAILED on both streams..."
  sse_wait_event p1 MATCHMAKING_FAILED
  sse_wait_event p2 MATCHMAKING_FAILED
  echo "--- p1 MATCHMAKING_FAILED data ---"; sse_event_data p1 MATCHMAKING_FAILED
  echo "Watch for: reason RETRIES_EXHAUSTED — the gRPC-reported kind"
  echo "(translateFailureReason's mapping in reportserver.go), NOT QUEUE_TIMEOUT (the"
  echo "sweep's natively-generated reason, scenario3b's case)."
  echo ""
  echo "NOTE — a real, observable finding, not a script artifact: PairingLoop.reenqueue"
  echo "puts both players back at their ORIGINAL queue scores after this failure"
  echo "(ADR-034), and their underlying problem (no users row) is permanent, not"
  echo "transient — so the VERY NEXT pairing-loop tick (500ms later) will immediately"
  echo "re-pick them and fail again, in an unbounded retry loop with no backoff. This"
  echo "script cancels both players below specifically to break that loop; a real"
  echo "client hitting this bug would see repeated MATCHMAKING_FAILED events forever"
  echo "until it gives up and calls DELETE itself. Worth its own tech-debt entry —"
  echo "there is currently no distinction anywhere in this system between a transient"
  echo "CreateMatchedGame failure (worth retrying) and a deterministic one (never will"
  echo "succeed, should stop retrying and surface differently)."
  mm_cancel p1 >/dev/null 2>&1 || true
  mm_cancel p2 >/dev/null 2>&1 || true
  echo "p1/p2 cancelled — retry loop broken."
}

scenario6() {
  echo "=== Scenario 6: Player already in an active shared-link game attempts to queue -> 409 ==="
  require curl; require jq; require uuidgen
  local uid resp gid qresp status body
  uid=$(uuidgen)
  resp=$(curl -s -X POST "$BASE_URL/games" -H "Content-Type: application/json" -d "{\"userID\":\"$uid\"}")
  gid=$(echo "$resp" | jq -r '.data.gameID')
  echo "Created shared-link game $gid for $uid (still WAITING_FOR_PLAYER — nobody's joined"
  echo "as Black, which does not matter: the active-game marker is written eagerly at"
  echo "CreateGame itself, per PHASE_3.md Step 3's checklist, before any opponent joins)."
  echo "Attempting to queue that SAME userID for matchmaking now..."
  qresp=$(curl -s -w '\n%{http_code}' -X POST "$BASE_URL/matchmaking/queue" \
    -H "Content-Type: application/json" -d "{\"userID\":\"$uid\"}")
  status=$(echo "$qresp" | tail -n1)
  body=$(echo "$qresp" | sed '$d')
  echo "$body" | jq .
  echo "HTTP $status"
  echo "Watch for: HTTP 409, code ALREADY_IN_ACTIVE_GAME, existingGame.gameID == $gid."
  echo "Confirming this player was NOT silently enqueued anyway (want: (nil)):"
  docker compose exec -T redis redis-cli ZSCORE 'matchmaking:queue:10+0' "$uid"
}

clean() {
  load_state
  for label in p1 p2 p3; do
    ws_disconnect "$label" >/dev/null 2>&1 || true
    sse_disconnect "$label" >/dev/null 2>&1 || true
  done
  rm -f "$STATE_DIR"/*.fifo
  rm -f "$STATE_FILE"
  echo "cleaned: killed background WS/SSE connections for p1/p2/p3, wiped state."
  echo "(regression-scenario r<N>a/r<N>b connections, if any are still open, are not"
  echo "individually tracked here — restart the shell or docker compose down if needed.)"
}

usage() {
cat <<EOF
chess-server Phase 3 (matchmaking) E2E helper — see
e2e-walkthrough/phase3_step7_e2e_walkthrough.md

Queue lifecycle:
  $0 queue <label> [seed=yes|no]   enqueue a player (seed=no skips user creation — see header)
  $0 status <label>
  $0 cancel <label>

SSE stream (GET /matchmaking/stream):
  $0 sse-connect <label>
  $0 sse-wait <label> <MATCH_FOUND|MATCHMAKING_FAILED> [timeoutSec]
  $0 sse-data <label> <event>       print the captured event's JSON payload
  $0 sse-tail <label>
  $0 sse-disconnect <label>

Matched-game WebSocket (after MATCH_FOUND, same shape as e2e-phase2.sh):
  $0 mwconnect <label>              connect directly using MATCH_FOUND's connectToken
  $0 reconnect <label>              reconnect via GET /games/:id/resolve, using the
                                     playerToken mwconnect captured (the fallback path)
  $0 wswait <label>
  $0 send <label> '<json>'
  $0 move <label> <SAN>
  $0 resign <label>
  $0 wstail <label>
  $0 wsdisconnect <label>

Introspection:
  $0 queue-card
  $0 queue-members
  $0 redis-keys
  $0 state
  $0 matched-count                  games with a non-null matchmaking_request_id

Cluster control:
  $0 kill <server1|server2|matchmaking-service>
  $0 start <server1|server2|matchmaking-service>

Guided scenarios (PHASE_3.md Step 7's checklist, in order):
  $0 scenario1     two players matched, connect normally, exchange a move
  $0 scenario2     three players — first two matched, third waits
  $0 scenario3a    voluntary cancel (DELETE)
  $0 scenario3b    queue-timeout sweep (~40s wall-clock wait)
  $0 scenario4 [N] concurrent pairing loops, no double-match (default N=10 pairs)
  $0 scenario5     CreateMatchedGame insert failure -> re-enqueue + MATCHMAKING_FAILED
  $0 scenario6     already in an active shared-link game -> 409

  $0 clean         kill background SSE/WS connections for p1/p2/p3, wipe state
EOF
}

main() {
  local cmd="${1:-help}"
  shift || true
  case "$cmd" in
    queue) mm_queue "${1:?label required}" "${2:-yes}" ;;
    status) mm_status "${1:?label required}" ;;
    cancel) mm_cancel "${1:?label required}" ;;
    sse-connect) sse_connect "${1:?label required}" ;;
    sse-wait) sse_wait_event "${1:?label required}" "${2:?event required}" "${3:-$SSE_WAIT_TIMEOUT}" ;;
    sse-data) sse_event_data "${1:?label required}" "${2:?event required}" ;;
    sse-tail) sse_tail "${1:?label required}" ;;
    sse-disconnect) sse_disconnect "${1:?label required}" ;;
    mwconnect) mm_ws_connect_from_match "${1:?label required}" ;;
    reconnect) mm_ws_reconnect_via_resolve "${1:?label required}" ;;
    wswait) ws_wait_ready "${1:?label required}" ;;
    send) ws_send "${1:?label required}" "${2:?json message required}" ;;
    move) move "${1:?label required}" "${2:?SAN required}" ;;
    resign) resign "${1:?label required}" ;;
    wstail) ws_tail "${1:?label required}" ;;
    wsdisconnect) ws_disconnect "${1:?label required}" ;;
    queue-card) queue_card ;;
    queue-members) queue_members ;;
    redis-keys) mm_redis_keys ;;
    state) mm_state ;;
    matched-count) matched_games_count ;;
    kill) kill_instance "${1:?server1|server2|matchmaking-service required}" ;;
    start) start_instance "${1:?server1|server2|matchmaking-service required}" ;;
    scenario1) scenario1 ;;
    scenario2) scenario2 ;;
    scenario3a) scenario3a ;;
    scenario3b) scenario3b ;;
    scenario4) scenario4 "${1:-10}" ;;
    scenario5) scenario5 ;;
    scenario6) scenario6 ;;
    clean) clean ;;
    help|-h|--help) usage ;;
    *) echo "unknown command: $cmd" >&2; usage; exit 1 ;;
  esac
}

main "$@"
