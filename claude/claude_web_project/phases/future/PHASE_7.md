# Phase 7 — Frontend

**Status: ⬜ Not Started**
**Prerequisite: Phase 6 all acceptance criteria met**

---

## Objective

Build a functional chess frontend that connects to the existing Go backend. Players can create games, share links, play chess in real time, view game history, and enter matchmaking. The UI is the delivery mechanism for demonstrating the backend — it is not the learning objective of this phase.

**The learning outcome of Phase 7:**
Connecting a real frontend to a production backend: WebSocket lifecycle management in the browser, handling reconnection in React, keeping client state synchronized with server-authoritative state, and understanding where Next.js rendering modes (server vs client) actually matter.

---

## Non-Goals

State this explicitly to avoid scope creep:

- This is not a UI/UX design exercise
- Pixel-perfect polish is not a goal
- Animation and transitions are not a goal
- Mobile responsiveness is nice-to-have, not required
- Accessibility is not in scope for Phase 7

The board must be playable. The UX must be functional. Beyond that, stop.

---

## Technology Stack

| Concern | Choice | Reason |
|---------|--------|--------|
| Framework | Next.js 14+ (App Router) | SSR for game history, static for UI pages, industry standard |
| WebSocket | Native browser WebSocket API | Gorilla speaks standard RFC 6455, no adapter needed |
| Chess board | react-chessboard | Do not write your own renderer |
| Client move validation | chess.js | UX only — legal move highlighting before server submission |
| State management | useReducer | WebSocket messages are already a state machine, no external library needed |
| HTTP client | fetch (native) | Plain REST calls to Go backend |
| Response validation | zod | Type-safe API responses, catches contract drift early |
| Styling | Tailwind CSS | Standard with Next.js, sufficient for functional UI |
| Language | TypeScript | Type safety across the WebSocket message protocol |

---

## Deployment Architecture

```
Browser
  │
  ├──── HTTPS/REST ────► Next.js on Vercel (frontend)
  │                          │
  │                          │ (SSR: game history, user pages)
  │
  └──── WSS/REST ──────► Go Server on VPS (backend)
                              │
                         PostgreSQL + Redis
```

**Next.js on Vercel:** Free tier, zero ops, handles CDN and SSL automatically.

**Go server on VPS:** DigitalOcean, Hetzner, or Fly.io. Same server from previous phases.

**WebSocket connects directly to Go server**, not through Next.js. Next.js does not proxy WebSocket connections. The browser opens a WebSocket connection directly to `wss://api.yourdomain.com/ws/game/:id?token=...`.

**CORS:** The Go server must allow requests from the Vercel domain. Configure this in Phase 7 Step 1 before anything else. Without CORS, every API call fails with a browser security error.

---

## Critical Design Constraint: Client vs Server Components

Next.js App Router has server components (run on server, no browser APIs) and client components (run in browser, can use useState, useEffect, WebSocket).

**Rule:** Anything that touches WebSocket, useState, or useEffect must be a client component (`"use client"` directive). The chess board and game logic are entirely client-side.

**Server components are appropriate for:**
- Game history page (fetch from DB via API, render on server)
- User profile page
- Static pages (home, about)

**Client components are required for:**
- The game board and all game interaction
- Matchmaking waiting room
- Any component that holds WebSocket state

Getting this wrong causes cryptic hydration errors. Decide the boundary upfront.

---

## Application Pages

```
/                          Home page (server component)
                           - Create game button
                           - Enter matchmaking button
                           - Recent games (if logged in)

/game/[id]                 Game page (client component)
                           - Chess board
                           - Clock display
                           - Resignation button
                           - Reconnection handling

/game/[id]/spectate        Spectator page (client component)
                           - Read-only chess board
                           - Move history

/matchmaking               Matchmaking waiting room (client component)
                           - Queue status
                           - Cancel button
                           - Redirect to /game/[id] on match

/history                   Game history (server component)
                           - List of completed games
                           - ELO change per game
                           - Link to game replay

/game/[id]/analysis        Game replay (client component)
                           - Step through move history
                           - No WebSocket needed — loads from API
```

---

## WebSocket Message Contract

The protocol is already defined in PHASE_1.md. The frontend consumes exactly those messages. Do not invent new message types. Do not modify the protocol. If the frontend needs something the protocol does not provide, that is a backend change requiring a new ADR, not a frontend workaround.

**TypeScript types for the protocol** (defined once, used everywhere):

```typescript
// Define these in lib/types.ts
// These are the source of truth for the client-side protocol

type ServerMessageType =
  | "GAME_STATE"
  | "MOVE_APPLIED"
  | "MOVE_REJECTED"
  | "GAME_OVER"
  | "OPPONENT_CONNECTED"
  | "OPPONENT_DISCONNECTED"
  | "OPPONENT_RECONNECTED"
  | "ERROR"
  | "PONG"

type ClientMessageType =
  | "MOVE"
  | "RESIGN"
  | "PING"

// Individual message shapes match PHASE_1.md exactly
// Field names, types, and units are authoritative from PHASE_1.md
// whiteTimeMs and blackTimeMs are milliseconds — not seconds
```

---

## WebSocket Lifecycle in the Browser

The browser WebSocket has the same lifecycle challenges as the server side. The frontend must handle:

**Connection states:**
```
CONNECTING → OPEN → CLOSING → CLOSED
```

**What the game component must handle:**
- Connection drop mid-game: show "Reconnecting..." overlay, attempt reconnect with same token
- Reconnect: open new WebSocket with same `?token=`, receive `GAME_STATE`, resume from current position
- Tab visibility change: browser may throttle or kill WebSocket on hidden tabs — detect with `document.visibilityState`
- Server-sent `OPPONENT_DISCONNECTED`: show "Opponent disconnected, waiting..." overlay
- `GAME_OVER`: show result modal, stop clock, disable board

**Reconnection strategy:**
Exponential backoff: retry after 1s, 2s, 4s, 8s, max 30s. After 5 failed attempts, show "Connection lost — refresh to rejoin" message. The server holds the game state for the abandonment window (60 seconds per Phase 1 spec) — reconnecting within that window resumes the game.

---

## Game State in the Browser

The game board state is driven entirely by WebSocket messages from the server. The client does not maintain its own authoritative game state — it only holds a local copy for rendering purposes.

**State shape the game component needs:**

```
fen           — current board position (from last GAME_STATE or MOVE_APPLIED)
turn          — whose turn (WHITE or BLACK)
playerColor   — which color this client is playing
moves         — move history array (for move list display)
whiteTimeMs   — white clock in milliseconds
blackTimeMs   — black clock in milliseconds
status        — WAITING / ACTIVE / COMPLETED / ABANDONED
outcome       — null or { winner, reason }
wsStatus      — CONNECTING / CONNECTED / RECONNECTING / DISCONNECTED
```

**Clock rendering:** The server sends clock values on every `MOVE_APPLIED`. The client renders a countdown locally between moves (purely cosmetic — the server is authoritative on timeout). Do not use client clock as game logic. If the client clock hits zero, do nothing except show visual urgency. Wait for server to send `GAME_OVER` with reason `TIMEOUT`.

---

## Client-Side Move Validation

chess.js on the client provides legal move highlighting and prevents sending obviously illegal moves.

**The flow:**
1. Player clicks a piece — chess.js computes legal destination squares — highlight them
2. Player drags piece to a square — chess.js validates the move locally
3. If legal locally: send `{ type: "MOVE", san: "e4" }` to server
4. Optimistically update the board position locally (for responsiveness)
5. Wait for server response:
   - `MOVE_APPLIED`: confirm the move, update clock values
   - `MOVE_REJECTED`: revert the optimistic update, show rejection reason

**Important:** The optimistic update in step 4 means the board briefly shows the move before server confirmation. This is correct UX. The revert in step 5 corrects any discrepancy. Never show a move as permanent until `MOVE_APPLIED` is received.

---

## REST API Calls

The frontend makes REST calls to the Go server for game creation and joining. These use `fetch` with zod validation on the response.

```
POST /games              → create game, get white's playerToken
POST /games/:id/join     → join game, get black's playerToken
GET  /games/:id          → check game exists before opening WebSocket
GET  /users/:id/games    → game history (paginated)
GET  /games/:id/pgn      → PGN download
GET  /health             → verify backend is reachable on page load
```

**Token storage:** Player tokens are stored in `localStorage` keyed by gameID (`playerToken:{gameID}`). On page load, the game page reads the token for that gameID from localStorage. If found, the player can reconnect. If not found, they are a spectator.

---

## Environment Variables (Next.js)

```env
NEXT_PUBLIC_API_URL=https://api.yourdomain.com
NEXT_PUBLIC_WS_URL=wss://api.yourdomain.com
```

`NEXT_PUBLIC_` prefix makes variables available in the browser bundle. Variables without this prefix are server-only (used for SSR API calls from Next.js server components).

---

## CORS Configuration on Go Server

Before any frontend work functions, the Go server needs CORS headers for the Vercel domain.

```go
// Add to chi middleware stack in internal/api/routes.go
r.Use(cors.Handler(cors.Options{
    AllowedOrigins:   []string{"https://your-app.vercel.app", "http://localhost:3000"},
    AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
    AllowedHeaders:   []string{"Content-Type", "Authorization"},
    AllowCredentials: false,
    MaxAge:           300,
}))
```

Use `go-chi/cors` (same ecosystem as the router, stdlib-compatible middleware).

**WebSocket and CORS:** The `gorilla/websocket` upgrader has a `CheckOrigin` function. In Phase 1 it was set to `return true` (allow all). In Phase 7, tighten it to check against your allowed origins list.

---

## Implementation Checklist

### Step 1: Go Server CORS + Origin Check
- [ ] Add `go-chi/cors` dependency to Go server
- [ ] Configure CORS for Vercel domain and localhost:3000
- [ ] Tighten `websocket.Upgrader.CheckOrigin` to allowed origins list
- [ ] Verify: `POST /games` from browser on localhost:3000 succeeds without CORS error
- [ ] Add ADR for CORS configuration decision

### Step 2: Next.js Project Setup
- [ ] `npx create-next-app@latest chess-client --typescript --tailwind --app`
- [ ] Create `lib/types.ts`: all WebSocket message types matching PHASE_1.md exactly
- [ ] Create `lib/api.ts`: typed fetch wrappers for all REST endpoints with zod validation
- [ ] Create `.env.local` with API and WS URLs
- [ ] Verify: `GET /health` call from Next.js page succeeds

### Step 3: WebSocket Hook
- [ ] `hooks/useGameWebSocket.ts`: custom hook encapsulating WebSocket lifecycle
- [ ] Connects to `WS_URL/ws/game/:id?token=`
- [ ] Manages connection state: CONNECTING / CONNECTED / RECONNECTING / DISCONNECTED
- [ ] Exposes: `sendMessage(msg)`, `connectionStatus`, `lastMessage`
- [ ] Implements reconnection with exponential backoff
- [ ] Cleans up connection on component unmount (no WebSocket leak)
- [ ] Unit tests: connection, reconnection, message handling (jest + mock WebSocket)

### Step 4: Game State Reducer
- [ ] `reducers/gameReducer.ts`: handles all server message types
- [ ] Initial state shape defined
- [ ] All message types handled: GAME_STATE, MOVE_APPLIED, MOVE_REJECTED, GAME_OVER, OPPONENT_*
- [ ] Unit tests: each action produces correct state transition
- [ ] Clock state updated on MOVE_APPLIED

### Step 5: Home Page
- [ ] Server component (no WebSocket, no useState)
- [ ] "Create Game" button → POST /games → store token in localStorage → redirect to /game/[id]
- [ ] "Find a Game" button → redirect to /matchmaking
- [ ] Clean, minimal layout

### Step 6: Game Page
- [ ] Client component (`"use client"`)
- [ ] On mount: read playerToken from localStorage for this gameID
- [ ] Open WebSocket connection via useGameWebSocket hook
- [ ] Feed messages into gameReducer
- [ ] Render react-chessboard with current FEN
- [ ] Board orientation: White sees board from White's perspective, Black from Black's
- [ ] Legal move highlighting via chess.js
- [ ] Optimistic move update on piece drop
- [ ] Revert on MOVE_REJECTED
- [ ] Clock display for both players (countdown locally, sync on MOVE_APPLIED)
- [ ] Resignation button → sends RESIGN message → confirm dialog first
- [ ] GAME_OVER modal: outcome, reason, ELO change (if available)
- [ ] Reconnecting overlay when wsStatus is RECONNECTING

### Step 7: Matchmaking Page
- [ ] Client component
- [ ] POST /matchmaking/queue on mount
- [ ] WebSocket or polling for MATCH_FOUND notification
- [ ] Display queue position or wait time
- [ ] Cancel button → DELETE /matchmaking/queue → redirect home
- [ ] On MATCH_FOUND: store playerToken in localStorage → redirect to /game/[id]

### Step 8: Spectator Page
- [ ] Client component
- [ ] No token required
- [ ] Opens WebSocket to /ws/game/:id/spectate
- [ ] Read-only board (no piece dragging)
- [ ] Move history list
- [ ] Both clocks displayed

### Step 9: Game History Page
- [ ] Server component (fetches from GET /users/:id/games on server)
- [ ] List of completed games with: opponent, outcome, ELO change, date, time control
- [ ] Link to /game/[id]/analysis for each game
- [ ] Pagination

### Step 10: Game Analysis Page
- [ ] Client component
- [ ] Loads full move history from GET /games/:id (no WebSocket)
- [ ] Step forward/backward through moves
- [ ] Displays board position at each step
- [ ] PGN download button → GET /games/:id/pgn

### Step 11: Deployment
- [ ] Deploy Next.js to Vercel
- [ ] Set NEXT_PUBLIC_API_URL and NEXT_PUBLIC_WS_URL in Vercel environment variables
- [ ] Update Go server CORS to allow Vercel production domain
- [ ] Verify: complete game from create to checkmate on production URLs
- [ ] Verify: reconnection works on production (close tab, reopen, game resumes)

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Two players on different machines complete a full game via the browser UI |
| 2 | Closing and reopening the browser tab mid-game resumes the game correctly |
| 3 | Illegal moves are highlighted as illegal before submission (chess.js) |
| 4 | MOVE_REJECTED reverts the board to the correct position |
| 5 | Clock counts down visually, syncs correctly on each move |
| 6 | GAME_OVER modal shows correct outcome and reason |
| 7 | Spectator page shows moves in real time without affecting the game |
| 8 | Game history page loads and displays correctly |
| 9 | All functionality works on production Vercel + VPS deployment |
| 10 | No WebSocket connection leak (opening and closing 10 game pages leaves no orphaned connections) |
