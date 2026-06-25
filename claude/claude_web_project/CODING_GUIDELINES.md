# Coding Guidelines

These guidelines are rules, not suggestions. Every piece of code written for this project follows them. When a guideline needs to change, it is updated here first, then applied consistently.

---

## 1. Error Handling

### Rule: Always wrap errors with context.

```go
// WRONG — naked error return
func (s *GameStore) GetGame(ctx context.Context, id string) (*Game, error) {
    row := s.pool.QueryRow(ctx, getGameQuery, id)
    if err := row.Scan(&game.ID /* ... */); err != nil {
        return nil, err  // ❌ No context about what failed or where
    }
    return &game, nil
}

// CORRECT — wrapped error with context
func (s *GameStore) GetGame(ctx context.Context, id string) (*Game, error) {
    row := s.pool.QueryRow(ctx, getGameQuery, id)
    if err := row.Scan(&game.ID /* ... */); err != nil {
        return nil, fmt.Errorf("GameStore.GetGame id=%s: %w", id, err)
    }
    return &game, nil
}
```

Always use `%w` (not `%v`) for error wrapping. `%w` preserves the error chain for `errors.Is()` and `errors.As()`.

### Rule: Distinguish "not found" from "error."

```go
// pgx returns pgx.ErrNoRows for empty results — check for it explicitly
if errors.Is(err, pgx.ErrNoRows) {
    return nil, ErrGameNotFound  // domain-level sentinel error
}
return nil, fmt.Errorf("GameStore.GetGame: %w", err)
```

Define sentinel errors in the domain package:
```go
// internal/game/errors.go
var (
    ErrGameNotFound    = errors.New("game not found")
    ErrGameNotActive   = errors.New("game is not active")
    ErrNotPlayersTurn  = errors.New("not this player's turn")
    ErrIllegalMove     = errors.New("illegal move")
)
```

### Rule: Never discard errors.

```go
// WRONG
conn.WriteMessage(websocket.TextMessage, data)  // ❌ error ignored

// CORRECT
if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
    slog.Error("failed to write message", "connID", c.ID, "error", err)
    // handle appropriately
}
```

The only acceptable error discard is `defer` cleanup where the error is unactionable and already logged:
```go
defer func() {
    if err := conn.Close(); err != nil {
        slog.Debug("connection close error", "connID", id, "error", err)
    }
}()
```

### Rule: No panic in library code.

`panic` is only acceptable in `main.go` for unrecoverable startup failures (e.g., cannot connect to database). Never in `internal/` packages.

```go
// WRONG — in internal package
func NewGameSession(id string) *GameSession {
    if id == "" {
        panic("id cannot be empty")  // ❌
    }
}

// CORRECT
func NewGameSession(id string) (*GameSession, error) {
    if id == "" {
        return nil, errors.New("game session id cannot be empty")
    }
    return &GameSession{ID: id}, nil
}
```

---

## 2. Context Propagation

### Rule: Every function that performs I/O takes `context.Context` as its first argument.

```go
// CORRECT signatures for I/O functions
func (s *GameStore) CreateGame(ctx context.Context, game *Game) error
func (s *MoveStore) SaveMove(ctx context.Context, move *Move) error
func (t *TokenService) VerifyPlayerToken(ctx context.Context, token string) (*PlayerClaims, error)

// WRONG — I/O without context
func (s *GameStore) CreateGame(game *Game) error  // ❌ cannot cancel, cannot trace
```

### Rule: Never store context in a struct.

```go
// WRONG
type GameStore struct {
    pool *pgxpool.Pool
    ctx  context.Context  // ❌ context belongs to the call, not the struct
}

// CORRECT
type GameStore struct {
    pool *pgxpool.Pool
}
func (s *GameStore) CreateGame(ctx context.Context, ...) error { ... }
```

### Rule: Pass request context through the call chain.

```go
func (h *GameHandler) CreateGame(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()  // use the request context
    game, err := h.store.CreateGame(ctx, params)  // pass it down
    // ...
}
```

---

## 3. Concurrency

### Rule: Document every mutex with what it protects.

```go
// CORRECT
type GameSession struct {
    // mu protects: state, board, playerWhite, playerBlack, whiteClock, blackClock
    mu          sync.RWMutex
    state       GameState
    board       *chess.Game
    playerWhite *ws.Connection
    playerBlack *ws.Connection
    whiteClock  time.Duration
    blackClock  time.Duration
}

// WRONG — undocumented mutex scope
type GameSession struct {
    mu    sync.RWMutex  // ❌ what does this protect?
    state GameState
    board *chess.Game
}
```

### Rule: Gorilla WebSocket connections are not concurrent-safe for writes.

Never call `conn.WriteMessage` from multiple goroutines. All writes go through the `send` channel and are consumed by a single write loop goroutine.

```go
// CORRECT — write via channel
func (c *Connection) Send(data []byte) error {
    select {
    case c.send <- data:
        return nil
    default:
        return ErrConnectionBufferFull
    }
}

// WRONG — direct write from arbitrary goroutine
conn.WriteMessage(websocket.TextMessage, data)  // ❌ race condition
```

### Rule: Use `sync.RWMutex` for read-heavy shared state.

Game registries and connection registries are read frequently (every message lookup) and written rarely (on connect/disconnect). Use `RWMutex` with `RLock` for reads.

### Rule: Never hold a lock while calling another function that acquires the same lock.

This is the deadlock pattern already identified in the registry code. The snapshot-then-release pattern is the standard solution:

```go
// CORRECT — snapshot under lock, operate outside lock
func (r *Registry) BroadcastAll(data []byte) {
    r.mu.RLock()
    conns := make([]*Connection, 0, len(r.connections))
    for _, c := range r.connections {
        conns = append(conns, c)
    }
    r.mu.RUnlock()

    for _, c := range conns {
        c.Send(data)  // Send may call Unregister internally — no deadlock
    }
}
```

---

## 4. Logging

### Rule: Use `log/slog` exclusively. No `fmt.Println`, no `log.Printf`.

```go
// CORRECT
slog.Info("game created", "gameID", game.ID, "playerWhiteID", game.PlayerWhiteID)
slog.Error("failed to save move", "gameID", gameID, "san", san, "error", err)
slog.Debug("player connected", "connID", connID, "gameID", gameID, "color", color)

// WRONG
fmt.Println("game created:", game.ID)       // ❌
log.Printf("game created: %s", game.ID)    // ❌
```

### Rule: Always include relevant IDs as structured fields.

Every log at Info level or above must include whatever subset of these IDs is available at the call site:

```go
// Fields to include when available:
"connID"    // WebSocket connection ID
"gameID"    // Game UUID
"userID"    // Player user ID
"color"     // "WHITE" or "BLACK"
"san"       // Move in Standard Algebraic Notation (for move-related logs)
```

### Rule: Log levels.

| Level | When to use |
|-------|-------------|
| Debug | Connection lifecycle events, heartbeats, internal state transitions |
| Info | Game created, game completed, player joined/left |
| Warn | Illegal move attempted, reconnection, game abandoned |
| Error | Database errors, unexpected state, panic recovery |

### Rule: Never log sensitive data.

JWT tokens, player tokens, and any credential must never appear in logs. Log the tokenHash (first 8 chars) if you need to trace a token.

---

## 5. Package Structure and Naming

### Rule: Package names are single lowercase words. No underscores, no plurals.

```
internal/ws      ✅
internal/game    ✅
internal/store   ✅
internal/auth    ✅
internal/chess   ✅
internal/api     ✅

internal/ws_server   ❌
internal/games       ❌
internal/handlers    ❌
```

### Rule: No package-level variables except for error sentinels and `var _ Interface = (*Type)(nil)` compile-time checks.

```go
// ACCEPTABLE — error sentinels
var ErrGameNotFound = errors.New("game not found")

// ACCEPTABLE — compile-time interface check
var _ EventBus = (*LocalEventBus)(nil)

// WRONG — global state
var db *pgxpool.Pool  // ❌ use dependency injection
var gameRegistry *GameRegistry  // ❌ pass through constructors
```

### Rule: Constructors use `New` prefix and return errors.

```go
// CORRECT
func NewGameSession(id string, playerWhiteID string) (*GameSession, error)
func NewGameRegistry() *GameRegistry  // acceptable without error if cannot fail
func NewGameStore(pool *pgxpool.Pool) *GameStore

// WRONG
func CreateGameSession(...) *GameSession  // inconsistent naming
func MakeGameStore(...) *GameStore        // inconsistent naming
```

---

## 6. Testing

### Rule: All tests use the race detector. Always.

```bash
go test -race ./...
```

A test that passes without `-race` but fails with `-race` is a bug. Fix the bug.

### Rule: Use table-driven tests for functions with multiple input/output cases.

```go
func TestValidateMove(t *testing.T) {
    tests := []struct {
        name    string
        fen     string
        san     string
        wantErr bool
    }{
        {"valid opening move", startFEN, "e4", false},
        {"illegal move", startFEN, "e5", true},  // e5 is black's, wrong turn
        {"en passant", enPassantFEN, "exd6", false},
        {"castling", castleFEN, "O-O", false},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := validator.ValidateAndApply(tt.fen, tt.san)
            if (err != nil) != tt.wantErr {
                t.Errorf("ValidateAndApply() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

### Rule: Store tests use a real PostgreSQL test database, not mocks.

Mocking the database in store tests defeats the purpose of the store layer tests. Use a separate `chess_test` database. Tests that require a database are integration tests and are tagged:

```go
//go:build integration
```

Run them with: `go test -tags integration -race ./...`

Unit tests (game logic, move validation, state machine) do not require a database and run without tags.

### Rule: WebSocket handler tests use `httptest.NewServer`.

```go
func TestWebSocketHandler(t *testing.T) {
    server := httptest.NewServer(router)
    defer server.Close()
    
    wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/game/" + gameID
    conn, _, err := websocket.DefaultDialer.Dial(wsURL + "?token=" + token, nil)
    // ...
}
```

---

## 7. HTTP API Conventions

### Rule: JSON responses always use a consistent envelope.

```go
// Success
{ "data": { ... } }

// Error
{ "error": { "code": "GAME_NOT_FOUND", "message": "game abc123 does not exist" } }
```

### Rule: HTTP status codes are used correctly.

| Code | When |
|------|------|
| 200 | Successful GET |
| 201 | Successful POST that creates a resource |
| 400 | Client error (invalid input, bad request body) |
| 401 | Missing or invalid authentication |
| 403 | Authenticated but not authorized |
| 404 | Resource not found |
| 409 | Conflict (e.g., game already has two players) |
| 500 | Server error (always log these) |

### Rule: Never return 200 with an error in the body.

```go
// WRONG — lying to the client
w.WriteHeader(http.StatusOK)
json.NewEncoder(w).Encode(map[string]string{"error": "game not found"})  // ❌

// CORRECT
w.WriteHeader(http.StatusNotFound)
json.NewEncoder(w).Encode(ErrorResponse{Error: ErrorDetail{Code: "GAME_NOT_FOUND", Message: "..."}})
```

---

## 8. Forbidden Patterns

These patterns are prohibited. No exceptions without a new ADR.

| Pattern | Why Forbidden |
|---------|--------------|
| `init()` functions | Hidden execution order, impossible to test in isolation |
| `interface{}` / `any` without justification | Loses type safety, masks bugs |
| Global mutable state | Prevents testing, creates hidden coupling |
| Client-side move validation as the only validation | Security vulnerability |
| Direct `conn.WriteMessage` from non-write-loop goroutines | Race condition |
| SQL outside the `store` package | Leaks persistence concerns into business logic |
| `time.Sleep` in tests | Flaky tests; use channels or explicit synchronization |
| Hardcoded strings for game states | Use typed constants |
| Ignoring `context.Context` cancellation | Goroutine leaks |

---

## 9. Go-Specific Rules

- **Named return values:** Only use them for documentation purposes in interface definitions. Never use them with naked `return`.
- **Error variable naming:** `err` for the most recent error. `createErr`, `saveErr` etc. if you need to distinguish multiple errors in scope.
- **Struct field ordering:** Public fields first, then private. Group related fields.
- **Interface size:** Prefer small interfaces. If an interface has more than 5 methods, question whether it should be split.
- **`defer` in loops:** Never use `defer` inside a loop. It does not execute until the function returns, not the loop iteration.

```go
// WRONG — defer in loop, closes don't happen until function returns
for _, row := range rows {
    f, _ := os.Open(row.filename)
    defer f.Close()  // ❌
}

// CORRECT — close explicitly in loop
for _, row := range rows {
    func() {
        f, _ := os.Open(row.filename)
        defer f.Close()  // ✅ closes after each iteration
        // use f
    }()
}
```
