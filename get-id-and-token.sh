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

echo

echo "WHITE: npx wscat -c \"ws://localhost:8080/ws/game/$GAME_ID?token=$WHITE_TOKEN\""
echo "BLACK: npx wscat -c \"ws://localhost:8080/ws/game/$GAME_ID?token=$BLACK_TOKEN\""