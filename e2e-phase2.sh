#!/usr/bin/env bash
# e2e-phase2.sh — Phase 2 multi-instance E2E helper for chess-server.
#
# Companion script to phase2_step11_e2e_walkthrough.md. Replaces manual
# curl/jq copy-paste (get-id-and-token.sh's approach) with persisted state
# across invocations and persistent, scriptable WebSocket connections, so
# each scenario in the walkthrough can be driven by short commands instead
# of hand-constructing URLs and pasting tokens before ConnectClaimsTTL
# expires.
#
# TIP: the first `npx wscat` invocation in a shell session can be slow
# (package resolution / occasional install). Run `npx --yes wscat --version`
# once by hand before your first `connect`/scenario run to warm the cache and
# confirm it doesn't need an interactive confirmation prompt (which would
# hang a backgrounded connection forever, since its stdin is a FIFO, not a
# TTY).
#
# State (GAME_ID, tokens, connect info, background PIDs) lives in
# /tmp/chess-e2e/state.env and is re-read on every invocation, since each
# `./e2e-phase2.sh <command>` call is a fresh process — this is what lets you
# run `./e2e-phase2.sh create`, then `./e2e-phase2.sh join`, then
# `./e2e-phase2.sh resolve both` as separate commands and have them agree on
# which game they're talking about.
#
# WebSocket connections are kept alive in a background subshell reading from
# a FIFO, so `./e2e-phase2.sh send white '...'` can push a message into an
# already-open connection at any later point — a fresh one-shot connection
# per message would look like a reconnect to the server (RegisterConnection
# occupied -> ReplaceConnection) and would not exercise "one open connection
# receives a broadcast triggered by the other," which is what Scenario 0
# actually needs to prove.
#
# Every scenario function waits for a real "Connected" signal from wscat
# (ws_wait_ready) before sending anything over a connection it just opened —
# NOT a fixed sleep. npx wscat's startup latency (node process spawn,
# package resolution, occasional install) is not bounded tightly enough for
# a guessed sleep duration to be reliable; a message sent before the
# connection is actually up sits unread with no error, which looks like a
# server bug (a stuck WAITING_FOR_PLAYER, or a resign that "didn't take")
# but isn't one.
#
# Requires: curl, jq, uuidgen, docker compose, and npx (for wscat) for the
# connect/send/move/resign commands specifically — everything else (create,
# join, resolve, status, owner, kill/start, logs) only needs curl/jq/docker.

set -euo pipefail

BASE_URL="${CHESS_BASE_URL:-http://localhost:8080}"
STATE_DIR="${CHESS_E2E_STATE_DIR:-/tmp/chess-e2e}"
STATE_FILE="$STATE_DIR/state.env"
LOG_DIR="./logs"
WS_READY_TIMEOUT="${CHESS_E2E_WS_READY_TIMEOUT:-20}" # seconds; npx cold start can be slow
mkdir -p "$STATE_DIR"

# --- state helpers -----------------------------------------------------

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

# --- game lifecycle ------------------------------------------------------

create_game() {
  require curl; require jq; require uuidgen
  local uid resp game_id token
  uid=$(uuidgen)
  resp=$(curl -s -X POST "$BASE_URL/games" \
    -H "Content-Type: application/json" -d "{\"userID\":\"$uid\"}")
  echo "$resp" | jq .
  game_id=$(echo "$resp" | jq -r '.data.gameID // empty')
  token=$(echo "$resp" | jq -r '.data.playerToken // empty')
  if [ -z "$game_id" ]; then
    echo "CreateGame failed" >&2
    return 1
  fi
  set_var GAME_ID "$game_id"
  set_var WHITE_USER_ID "$uid"
  set_var WHITE_TOKEN "$token"
  echo "GAME_ID=$game_id"
}

join_game() {
  require curl; require jq; require uuidgen
  load_state
  if [ -z "${GAME_ID:-}" ]; then
    echo "No GAME_ID in state — run: $0 create" >&2
    return 1
  fi
  local uid resp token
  uid=$(uuidgen)
  resp=$(curl -s -X POST "$BASE_URL/games/$GAME_ID/join" \
    -H "Content-Type: application/json" -d "{\"userID\":\"$uid\"}")
  echo "$resp" | jq .
  token=$(echo "$resp" | jq -r '.data.playerToken // empty')
  if [ -z "$token" ]; then
    echo "JoinGame failed" >&2
    return 1
  fi
  set_var BLACK_USER_ID "$uid"
  set_var BLACK_TOKEN "$token"
  echo "joined as BLACK"
}

resolve_one() {
  local color="$1" # white|black
  load_state
  local token_var="${color^^}_TOKEN"
  local token="${!token_var:-}"
  if [ -z "$token" ]; then
    echo "No token for $color yet" >&2
    return 1
  fi
  local resp connect wspath label
  resp=$(curl -s "$BASE_URL/games/$GAME_ID/resolve" -H "Authorization: Bearer $token")
  echo "$resp" | jq .
  connect=$(echo "$resp" | jq -r '.data.connectToken // empty')
  wspath=$(echo "$resp" | jq -r '.data.wsPath // empty')
  label=$(echo "$resp" | jq -r '.data.instanceLabel // empty')
  if [ -z "$connect" ]; then
    echo "Resolve failed for $color" >&2
    return 1
  fi
  set_var "${color^^}_CONNECT" "$connect"
  set_var "${color^^}_WS_PATH" "$wspath"
  set_var "${color^^}_INSTANCE" "$label"
  # http(s):// -> ws(s):// via substring replace; precomputed here so
  # `connect` can dial immediately without re-deriving it under TTL pressure.
  set_var "${color^^}_WS_URL" "${BASE_URL/http/ws}${wspath}?token=${connect}"
  echo "$color -> instance=$label"
}

resolve_both() {
  resolve_one white
  resolve_one black
  load_state
  if [ "$WHITE_INSTANCE" = "$BLACK_INSTANCE" ]; then
    echo "OK: both resolved to the same instance ($WHITE_INSTANCE)"
  else
    echo "MISMATCH: white=$WHITE_INSTANCE black=$BLACK_INSTANCE  <-- co-location broken"
  fi
}

new_game() {
  # Equivalent of get-id-and-token.sh's whole flow, state-persisted so every
  # other command below can build on it without re-pasting anything.
  create_game
  join_game
  resolve_both
}

# --- WebSocket connections (persistent, background, FIFO-fed) -----------

ws_connect() {
  local color="$1"
  require npx
  load_state
  local url_var="${color^^}_WS_URL"
  local url="${!url_var:-}"
  if [ -z "$url" ]; then
    echo "No resolved URL for $color — run: $0 resolve $color" >&2
    return 1
  fi

  local fifo="$STATE_DIR/${color}.fifo"
  local log="$STATE_DIR/${color}.log"
  ws_disconnect "$color" >/dev/null 2>&1 || true
  rm -f "$fifo"
  mkfifo "$fifo"
  : > "$log"

  # Open the FIFO read-write (fd 3) in the background subshell first — this
  # does not block on Linux even with no other party yet (unlike opening
  # write-only or read-only alone), and because this fd stays open for the
  # life of the subshell, the FIFO never sees "zero writers," so wscat's
  # stdin never hits EOF just because a short-lived `send` invocation opens
  # and closes the FIFO for writing later. Any bytes written before wscat
  # itself starts reading simply sit in the kernel pipe buffer — they are
  # NOT lost, just delayed, which is why ws_wait_ready (waiting for wscat's
  # own readiness line) rather than a fixed sleep is what actually matters
  # here.
  (
    exec 3<>"$fifo"
    # npx --yes wscat -c "$url" <&3 > "$log" 2>&1
    wscat -c "$url" <&3 > "$log" 2>&1
  ) &
  local pid=$!
  disown "$pid" 2>/dev/null || true

  set_var "${color^^}_WS_PID" "$pid"
  set_var "${color^^}_WS_FIFO" "$fifo"
  set_var "${color^^}_WS_LOG" "$log"
  echo "$color connecting in background (pid $pid)..."
  echo "   tail:       $0 tail $color"
  echo "   send:       $0 send $color '{\"type\":\"MOVE\",\"san\":\"e4\"}'"
  echo "   disconnect: $0 disconnect $color"
}

# Waits for wscat's own "Connected" line to appear in the connection's log —
# NOT a fixed sleep. npx startup (node spawn, package resolution, occasional
# install) is not reliably bounded by a short guessed delay; sending a
# message before the connection is actually up sits silently unread, which
# looks exactly like a server-side bug but isn't one. Call this after every
# ws_connect before move/send/resign.
ws_wait_ready() {
  local color="$1" timeout="${2:-$WS_READY_TIMEOUT}"
  load_state
  local log_var="${color^^}_WS_LOG"
  local log="${!log_var:-}"
  if [ -z "$log" ]; then
    echo "no log for $color — run: $0 connect $color" >&2
    return 1
  fi
  local start=$SECONDS
  while (( SECONDS - start < timeout )); do
    if grep -qi -E "Connected|GAME_STATE" "$log" 2>/dev/null; then
      echo "$color: connection confirmed ready ($(( SECONDS - start ))s)."
      return 0
    fi
    sleep 0.3
  done
  echo "WARNING: $color's WebSocket did not report 'Connected' within ${timeout}s." >&2
  echo "         Check the raw log for an npx install/confirmation prompt or a" >&2
  echo "         connection error (e.g. CONNECT_TOKEN_EXPIRED): $0 tail $color" >&2
  return 1
}

ws_send() {
  local color="$1" msg="$2"
  load_state
  local fifo_var="${color^^}_WS_FIFO"
  local fifo="${!fifo_var:-}"
  if [ -z "$fifo" ] || [ ! -p "$fifo" ]; then
    echo "No active connection for $color — run: $0 connect $color" >&2
    return 1
  fi
  echo "$msg" > "$fifo"
}

move() { ws_send "$1" "{\"type\":\"MOVE\",\"san\":\"$2\"}"; }
resign() { ws_send "$1" '{"type":"RESIGN"}'; }

ws_tail() {
  local color="$1"
  load_state
  local log_var="${color^^}_WS_LOG"
  local log="${!log_var:-}"
  if [ -z "$log" ]; then
    echo "No log for $color yet — run: $0 connect $color" >&2
    return 1
  fi
  tail -n 40 -f "$log"
}

ws_disconnect() {
  local color="$1"
  load_state
  local pid_var="${color^^}_WS_PID"
  local pid="${!pid_var:-}"
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  unset_var "${color^^}_WS_PID"
}

# --- introspection ---------------------------------------------------------

status() {
  load_state
  curl -s "$BASE_URL/games/$GAME_ID" | jq .
}

get_owner_raw() {
  load_state
  docker compose exec -T redis redis-cli GET "game:$GAME_ID" | tr -d '\r\n"'
}

owner() { get_owner_raw; echo; }

alive() {
  local label="$1"
  docker compose exec -T redis redis-cli EXISTS "instance_alive:$label"
}

redis_keys() {
  echo "--- game:* ---"
  docker compose exec -T redis redis-cli KEYS 'game:*'
  echo "--- instance_alive:* ---"
  docker compose exec -T redis redis-cli KEYS 'instance_alive:*'
}

state() {
  load_state
  env | grep -E '^(GAME_ID|WHITE_|BLACK_|DEAD_INSTANCE|LOGS_)=' | sort || true
}

# --- cluster control ---------------------------------------------------

kill_instance() { docker compose stop "$1"; }
start_instance() { docker compose start "$1"; }
redis_down() { docker compose stop redis; }
redis_up() { docker compose start redis; }

# --- docker compose log capture ----------------------------------------

logs_start() {
  mkdir -p "$LOG_DIR"
  local file="$LOG_DIR/cluster-$(date +%Y%m%d-%H%M%S).log"
  docker compose logs -f --no-color --timestamps > "$file" 2>&1 &
  local pid=$!
  disown "$pid" 2>/dev/null || true
  set_var LOGS_PID "$pid"
  set_var LOGS_FILE "$file"
  echo "capturing docker compose logs -> $file (pid $pid)"
}

logs_stop() {
  load_state
  [ -n "${LOGS_PID:-}" ] && kill "$LOGS_PID" 2>/dev/null || true
  echo "stopped. file: ${LOGS_FILE:-none captured}"
}

logs_grep() {
  local pattern="$1"
  load_state
  if [ -z "${LOGS_FILE:-}" ]; then
    echo "no active log capture — run: $0 logs-start" >&2
    return 1
  fi
  grep -n "$pattern" "$LOGS_FILE" || echo "no matches for: $pattern"
}

# --- guided scenarios (mirrors phase2_step11_e2e_walkthrough.md) --------

scenario0() {
  echo "=== Scenario 0: Co-location ==="
  new_game
  echo "Connecting both players now (ConnectClaimsTTL is tight — dialing immediately)..."
  ws_connect white
  ws_connect black
  ws_wait_ready white
  ws_wait_ready black
  load_state
  echo "--- white log ---"; tail -n 20 "$WHITE_WS_LOG"
  echo "--- black log ---"; tail -n 20 "$BLACK_WS_LOG"
  echo "Watch for: White sees GAME_STATE then OPPONENT_CONNECTED; Black sees GAME_STATE (ACTIVE)."
  pause
  echo "Sending e4 as White..."
  move white e4
  sleep 1
  load_state
  echo "--- white log (tail) ---"; tail -n 5 "$WHITE_WS_LOG"
  echo "--- black log (tail) ---"; tail -n 5 "$BLACK_WS_LOG"
  echo "Watch for: MOVE_APPLIED in BOTH logs. If Black's log doesn't show it, co-location is broken."
}

scenario1() {
  echo "=== Scenario 1: Failover ==="
  load_state
  if [ -z "${GAME_ID:-}" ]; then echo "run scenario0 first"; return 1; fi
  local owner_id; owner_id=$(get_owner_raw)
  echo "Current owner: $owner_id"
  echo "Stopping $owner_id..."
  kill_instance "$owner_id"
  sleep 2
  echo "Re-resolving both players..."
  resolve_both
  load_state
  if [ "$WHITE_INSTANCE" = "$owner_id" ]; then
    echo "FAIL: still resolving to the dead instance"
  else
    echo "OK: failed over to $WHITE_INSTANCE"
  fi
  echo "Reconnecting both..."
  ws_disconnect white; ws_disconnect black
  ws_connect white; ws_connect black
  ws_wait_ready white
  ws_wait_ready black
  load_state
  echo "Watch for: GAME_STATE with the prior move (e4) already applied, Black to move."
  tail -n 20 "$WHITE_WS_LOG"
  set_var DEAD_INSTANCE "$owner_id"
}

scenario2() {
  echo "=== Scenario 2: Recovered instance must NOT re-acquire (split-brain test) ==="
  load_state
  if [ -z "${DEAD_INSTANCE:-}" ]; then echo "run scenario1 first"; return 1; fi
  echo "Restarting $DEAD_INSTANCE..."
  start_instance "$DEAD_INSTANCE"
  sleep 5
  echo "Redis owner now: $(get_owner_raw)"
  echo "Watch for: still the survivor, NOT $DEAD_INSTANCE."
  resolve_one white
  load_state
  if [ "$WHITE_INSTANCE" = "$DEAD_INSTANCE" ]; then
    echo "SPLIT-BRAIN BUG: recovered instance re-acquired ownership"
  else
    echo "OK: recovered instance did not re-acquire ($WHITE_INSTANCE still owns it)"
  fi
}

scenario3() {
  echo "=== Scenario 3: One player never reconnects (abandonment) ==="
  new_game
  load_state
  local owner_id; owner_id=$(get_owner_raw)
  ws_connect white; ws_connect black
  ws_wait_ready white
  ws_wait_ready black
  echo "Stopping owner $owner_id (only White will reconnect)..."
  kill_instance "$owner_id"
  ws_disconnect white; ws_disconnect black
  resolve_one white
  ws_connect white
  ws_wait_ready white
  echo "Black will NOT reconnect. Waiting 65s for the abandonment timer (real wall-clock wait)..."
  sleep 65
  status
  echo "Watch for: status COMPLETED, outcome WHITE, outcomeReason ABANDONED"
}

scenario4() {
  echo "=== Scenario 4: Redis down mid-game ==="
  new_game
  ws_connect white; ws_connect black
  ws_wait_ready white
  ws_wait_ready black
  echo "Stopping Redis..."
  redis_down
  echo "Sending a move — must still work (gameplay never touches Redis)..."
  move white e4
  sleep 1
  load_state
  tail -n 5 "$WHITE_WS_LOG"; tail -n 5 "$BLACK_WS_LOG"
  echo "Attempting a NEW resolve — must fail cleanly, not hang, not crash the instance."
  curl -s -w "\nHTTP %{http_code}\n" "$BASE_URL/games/$GAME_ID/resolve" \
    -H "Authorization: Bearer $WHITE_TOKEN"
  docker compose ps server1 server2
  echo "Restoring Redis..."
  redis_up
  sleep 3
  resolve_one white
}

scenario5() {
  echo "=== Scenario 5: Reconnect to an already-completed game ==="
  new_game
  ws_connect white; ws_connect black
  ws_wait_ready white
  ws_wait_ready black
  echo "White resigns..."
  resign white
  sleep 1.5
  status
  echo "Watch for: status COMPLETED, outcome BLACK, outcomeReason RESIGNATION"
  ws_disconnect white; ws_disconnect black
  echo "Resolving AFTER game end (Black)..."
  resolve_one black
  ws_connect black
  ws_wait_ready black
  load_state
  tail -n 10 "$BLACK_WS_LOG"
  echo "Watch for: GAME_STATE delivered immediately with the correct terminal state, not an error."
  echo "Owning instance now (TD-P2-004 — session stays registered indefinitely, by design):"
  get_owner_raw; echo
}

scenario6() {
  echo "=== Scenario 6: Opponent never joins, creator disconnects — ABORTED (ADR-029) ==="
  create_game
  echo "Deliberately NOT joining as Black — nobody ever joins this game."
  resolve_one white
  ws_connect white
  ws_wait_ready white
  load_state
  echo "--- white log ---"; tail -n 10 "$WHITE_WS_LOG"
  echo "Watch for: GAME_STATE with status WAITING_FOR_PLAYER above."
  echo "Disconnecting White — simulating the creator giving up with no opponent ever having joined..."
  ws_disconnect white
  echo "Waiting 65s for the abandonment timer (real wall-clock wait, same timer as"
  echo "scenario3, different terminal outcome since this game never reached ACTIVE)..."
  sleep 65
  status
  echo "Watch for: status ABORTED, outcome null, outcomeReason null."
  echo "Not COMPLETED, not ABANDONED — both imply the game actually started."
  echo "If still WAITING_FOR_PLAYER, check migration 004 actually ran: docker compose logs chess_migrate"
}

regress() {
  # Not in the original walkthrough. Direct E2E-level regression check for
  # ADR-028 (CRITICAL-1/CRITICAL-2): fires N rapid create+join cycles, which
  # round-robins CreateGame and JoinGame across both instances repeatedly.
  # A single manual trial (Scenario 0) can pass by luck even with the old
  # bug present, since round-robin only lands cross-instance ~50% of the
  # time — this loop is what actually exercises that.
  local n="${1:-10}"
  echo "=== Regression: $n rapid create+join cycles across instances (CRITICAL-1/2) ==="
  local fails=0
  for i in $(seq 1 "$n"); do
    if ! create_game >/dev/null 2>&1; then fails=$((fails+1)); continue; fi
    if ! join_game >/dev/null 2>&1; then fails=$((fails+1)); continue; fi
  done
  echo "join failures: $fails / $n  (want 0 — CRITICAL-1 regression check)"
  load_state
  if [ -n "${LOGS_FILE:-}" ]; then
    local count
    count=$(grep -c "lost ownership renewal" "$LOGS_FILE" || true)
    echo "occurrences of 'lost ownership renewal' in ${LOGS_FILE}: $count  (want 0 — CRITICAL-2 regression check)"
  else
    echo "no log capture active — run '$0 logs-start' BEFORE this command for an automated check"
  fi
}

clean() {
  load_state
  ws_disconnect white >/dev/null 2>&1 || true
  ws_disconnect black >/dev/null 2>&1 || true
  [ -n "${LOGS_PID:-}" ] && kill "${LOGS_PID}" 2>/dev/null || true
  rm -f "$STATE_DIR"/*.fifo
  rm -f "$STATE_FILE"
  echo "cleaned: killed background connections/log capture, wiped state."
}

usage() {
cat <<EOF
chess-server Phase 2 E2E helper — see phase2_step11_e2e_walkthrough.md

Setup:
  $0 create                     create a game (White)
  $0 join                       join as Black
  $0 new                        create + join + resolve both (get-id-and-token.sh, scripted)
  $0 resolve <white|black|both>

Live connections (persistent, background, via a FIFO into wscat):
  $0 connect <white|black>
  $0 wait <white|black>          block until the connection reports ready
  $0 send <white|black> '<json>'
  $0 move <white|black> <SAN>
  $0 resign <white|black>
  $0 tail <white|black>
  $0 disconnect <white|black>

Introspection:
  $0 status
  $0 owner
  $0 alive <instanceLabel>
  $0 redis-keys
  $0 state

Cluster control:
  $0 kill <server1|server2>
  $0 start <server1|server2>
  $0 redis-down
  $0 redis-up

Docker log capture:
  $0 logs-start
  $0 logs-stop
  $0 logs-grep '<pattern>'

Guided scenarios:
  $0 scenario0     co-location
  $0 scenario1     failover                       (after scenario0)
  $0 scenario2     recovered must not re-acquire  (after scenario1)
  $0 scenario3     one-sided abandonment (~65s wall-clock wait)
  $0 scenario4     redis down mid-game
  $0 scenario5     reconnect to a completed game
  $0 scenario6     opponent never joins, creator disconnects — ABORTED (~65s wait)
  $0 regress [N]   rapid create+join cycles — CRITICAL-1/2 regression check (default N=10)

  $0 clean         kill background connections/log capture, wipe state
EOF
}

main() {
  local cmd="${1:-help}"
  shift || true
  case "$cmd" in
    create) create_game ;;
    join) join_game ;;
    new) new_game ;;
    resolve)
      case "${1:-both}" in
        white) resolve_one white ;;
        black) resolve_one black ;;
        both) resolve_both ;;
        *) echo "usage: $0 resolve <white|black|both>" >&2; exit 1 ;;
      esac
      ;;
    connect) ws_connect "${1:?usage: $0 connect <white|black>}" ;;
    wait) ws_wait_ready "${1:?usage: $0 wait <white|black>}" ;;
    send) ws_send "${1:?color required}" "${2:?json message required}" ;;
    move) move "${1:?color required}" "${2:?SAN required}" ;;
    resign) resign "${1:?color required}" ;;
    tail) ws_tail "${1:?color required}" ;;
    disconnect) ws_disconnect "${1:?color required}" ;;
    status) status ;;
    owner) owner ;;
    alive) alive "${1:?instanceLabel required}" ;;
    redis-keys) redis_keys ;;
    state) state ;;
    kill) kill_instance "${1:?server1|server2 required}" ;;
    start) start_instance "${1:?server1|server2 required}" ;;
    redis-down) redis_down ;;
    redis-up) redis_up ;;
    logs-start) logs_start ;;
    logs-stop) logs_stop ;;
    logs-grep) logs_grep "${1:?pattern required}" ;;
    scenario0) scenario0 ;;
    scenario1) scenario1 ;;
    scenario2) scenario2 ;;
    scenario3) scenario3 ;;
    scenario4) scenario4 ;;
    scenario5) scenario5 ;;
    scenario6) scenario6 ;;
    regress) regress "${1:-10}" ;;
    clean) clean ;;
    help|-h|--help) usage ;;
    *) echo "unknown command: $cmd" >&2; usage; exit 1 ;;
  esac
}

main "$@"
