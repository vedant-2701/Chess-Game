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
echo "WHITE_TOKEN=$WHITE_TOKEN (playerToken — used only to call /resolve now, never dialed directly)"
echo "BLACK_TOKEN=$BLACK_TOKEN"

echo

# PHASE_2.md Step 5/8: playerToken no longer dials the WebSocket directly.
# Each player must call GET /games/:id/resolve (Authorization: Bearer
# <playerToken>) to obtain a short-lived connectToken + wsPath, THEN dial
# that. This script does both resolve calls now, rather than the old
# single-step direct-token-in-URL flow.
curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $WHITE_TOKEN" | tee /tmp/resolve_white.json | jq .
WHITE_CONNECT_TOKEN=$(jq -r '.data.connectToken' /tmp/resolve_white.json)
WHITE_WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_white.json)

curl -s http://localhost:8080/games/$GAME_ID/resolve \
  -H "Authorization: Bearer $BLACK_TOKEN" | tee /tmp/resolve_black.json | jq .
BLACK_CONNECT_TOKEN=$(jq -r '.data.connectToken' /tmp/resolve_black.json)
BLACK_WS_PATH=$(jq -r '.data.wsPath' /tmp/resolve_black.json)

echo
echo "WHITE: npx wscat -c \"ws://localhost:8080${WHITE_WS_PATH}?token=$WHITE_CONNECT_TOKEN\""
echo "BLACK: npx wscat -c \"ws://localhost:8080${BLACK_WS_PATH}?token=$BLACK_CONNECT_TOKEN\""
echo
echo "NOTE: connectToken expires in ~10s (ConnectClaims TTL, ADR-022) — dial"
echo "immediately after resolving, or re-run the resolve curl calls above if"
echo "you get CONNECT_TOKEN_EXPIRED."
