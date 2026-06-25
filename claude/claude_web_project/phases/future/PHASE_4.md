# Phase 4 — ELO Rating and Game History

**Status: ⬜ Not Started**
**Prerequisite: Phase 3 all acceptance criteria met**

---

## Objective

Add ELO ratings computed after every game, game history browsable per player, and PGN export. The key learning is not the ELO formula — it is understanding *when* to compute it and *how* to handle failure in async processing.

**The single most important learning outcome of Phase 4:**
Not every computation needs to happen synchronously on the critical path. Deferred async processing is a fundamental pattern. Understanding idempotency — what happens when a job runs twice — is what separates correct async systems from broken ones.

---

## The Problem This Phase Solves

When a game ends, the naive implementation is:

```go
// In game completion handler:
updateGameStatus(COMPLETED)
computeELO(playerWhite, playerBlack, outcome)  // synchronous
respondToClient()
```

This is wrong for three reasons:
1. ELO computation failure blocks game completion — a database error in ELO rolls back the game result
2. ELO computation is not time-critical — players can wait 5 seconds to see their new rating
3. If you add more post-game computation (achievements, statistics, notifications), the critical path grows unboundedly

The correct pattern: game ends → emit event → respond to client → worker picks up event → compute ELO asynchronously.

---

## Scope

### In Scope

- ELO computed asynchronously after every game via a job queue
- ELO stored per user, updated after every game
- Game history: list of completed games per player with outcomes and ELO change
- PGN export for any completed game
- Multiple time controls added to queue (3+0, 5+0, 10+0, 10+5)
- Database indexes added and evaluated via EXPLAIN ANALYZE
- ELO-based matchmaking: queue pairs players within ±200 ELO (expand to ±400 after 30 seconds)

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| Real-time ELO during game | Not how chess ELO works |
| Leaderboards | Not a system design concept for this project phase |
| ELO history graph | UI feature, not backend |
| Chess titles (CM, FM, IM, GM) | Not relevant to system design |

---

## Key Technical Challenges

### Challenge 1: Job Queue Design

Phase 4 introduces a job queue for ELO computation. Two options:

**Option A: Redis list as job queue**
```
LPUSH elo:jobs '{"gameID":"...", "whiteID":"...", "blackID":"...", "outcome":"WHITE"}'
BRPOP elo:jobs 0  ←  blocking pop, worker waits for jobs
```
- Simple, no new infrastructure
- At-least-once delivery if worker tracks job completion
- Message lost if worker crashes between BRPOP and job completion

**Option B: PostgreSQL-based job queue (pg-based)**
```sql
CREATE TABLE elo_jobs (
    id         BIGSERIAL PRIMARY KEY,
    game_id    UUID NOT NULL,
    status     TEXT DEFAULT 'PENDING',
    attempts   INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    locked_at  TIMESTAMPTZ
);
```
- Durable: survives Redis restart
- Requires polling (SELECT FOR UPDATE SKIP LOCKED)
- No new infrastructure

**Recommendation to evaluate at phase start:** PostgreSQL job queue for Phase 4. It is durable, requires no new infrastructure, and `SELECT FOR UPDATE SKIP LOCKED` teaches an important PostgreSQL pattern. Redis job queue is appropriate when throughput exceeds PostgreSQL's capacity — not the case here.

### Challenge 2: Idempotency

What if the ELO worker crashes after updating White's ELO but before updating Black's ELO?

On retry:
- White's ELO is updated twice
- Black's ELO is updated once

This is wrong. The job must be idempotent — running it twice produces the same result as running it once.

**Solution:** Wrap both ELO updates in a single database transaction. Use a `processed` flag on the job. Check the flag before processing. If already processed, skip.

```sql
BEGIN;
UPDATE users SET elo = $1 WHERE id = $2;
UPDATE users SET elo = $3 WHERE id = $4;
UPDATE elo_jobs SET status = 'COMPLETED' WHERE id = $5;
COMMIT;
```

If the transaction fails, the job status remains `PENDING` and the worker retries. On retry, the transaction either succeeds fully or fails fully. No partial updates.

### Challenge 3: ELO-Based Matchmaking

When ELO exists, FIFO matchmaking produces bad games. A 2000-rated player matched against a 600-rated player is not a game — it is a guaranteed win.

ELO-based matchmaking: match players within ±200 ELO. If no match after 30 seconds, expand range to ±400. After 60 seconds, expand to ±600.

This requires changing the Redis queue structure from a single sorted set to multiple sets by ELO bucket, or using a different pairing algorithm.

### Challenge 4: Database Indexing

Game history queries are the first real read-heavy workload. Without indexes:

```sql
SELECT * FROM games WHERE player_white_id = $1 OR player_black_id = $1 ORDER BY created_at DESC;
```

This is a full table scan on every game history request. With 100,000 games, this is unacceptably slow.

**Phase 4 learning exercise:** Run `EXPLAIN ANALYZE` before and after adding indexes. Measure the actual query time difference.

---

## ELO Formula

```go
func expectedScore(playerELO, opponentELO float64) float64 {
    return 1.0 / (1.0 + math.Pow(10, (opponentELO-playerELO)/400.0))
}

func newELO(currentELO, expectedScore, actualScore float64, kFactor float64) float64 {
    return currentELO + kFactor*(actualScore-expectedScore)
}
// K-factor: 32 for new players (<30 games), 16 for established players
// actualScore: 1.0 = win, 0.5 = draw, 0.0 = loss
```

This is the entire ELO computation. It is trivial. The system design around it is the learning objective, not the formula.

---

## New Database Schema Additions

```sql
-- Add ELO to users
ALTER TABLE users ADD COLUMN elo INT NOT NULL DEFAULT 1200;
ALTER TABLE users ADD COLUMN games_played INT NOT NULL DEFAULT 0;

-- ELO job queue
CREATE TABLE elo_jobs (
    id          BIGSERIAL PRIMARY KEY,
    game_id     UUID NOT NULL REFERENCES games(id),
    status      TEXT NOT NULL DEFAULT 'PENDING'
                CHECK (status IN ('PENDING', 'PROCESSING', 'COMPLETED', 'FAILED')),
    attempts    INT NOT NULL DEFAULT 0,
    error_msg   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

CREATE INDEX idx_elo_jobs_pending ON elo_jobs(status, created_at) WHERE status = 'PENDING';

-- ELO history for each game
ALTER TABLE games ADD COLUMN white_elo_before INT;
ALTER TABLE games ADD COLUMN black_elo_before INT;
ALTER TABLE games ADD COLUMN white_elo_after  INT;
ALTER TABLE games ADD COLUMN black_elo_after  INT;

-- Indexes for game history queries
CREATE INDEX idx_games_white_player ON games(player_white_id, created_at DESC);
CREATE INDEX idx_games_black_player ON games(player_black_id, created_at DESC);
```

---

## New Endpoints

```
GET /users/:id/games
  Query: ?page=1&limit=20
  Response: paginated list of games with outcomes and ELO changes

GET /users/:id/elo
  Response: { "elo": 1450, "gamesPlayed": 87 }

GET /games/:id/pgn
  Response: PGN text (plain text, not JSON)
  Content-Type: text/plain

GET /matchmaking/queue?timeControl=10+0
  (Updated to support multiple time controls)
```

---

## Implementation Checklist

### Step 1: Database Schema Updates
- [ ] Migration: add ELO columns to users
- [ ] Migration: create elo_jobs table with index
- [ ] Migration: add ELO snapshot columns to games
- [ ] Migration: add game history indexes
- [ ] Run EXPLAIN ANALYZE on game history query before and after indexes — document results

### Step 2: ELO Computation
- [ ] `internal/elo/calculator.go`: expectedScore, newELO, KFactor logic
- [ ] Unit tests: verify ELO calculations against known values

### Step 3: Job Queue
- [ ] `internal/jobs/elo_worker.go`:
  - [ ] `Worker` struct
  - [ ] `Start(ctx)` — polling loop with SELECT FOR UPDATE SKIP LOCKED
  - [ ] `processJob(ctx, job)` — compute ELO, update users, mark job complete (single transaction)
  - [ ] Retry logic: max 3 attempts, exponential backoff
  - [ ] `Stop()` — clean shutdown
- [ ] Job enqueue: called from game completion handler (after game status set to COMPLETED)
- [ ] Integration tests:
  - [ ] Job enqueued on game completion
  - [ ] ELO updated correctly after job runs
  - [ ] Crashed worker (simulate): job retried, ELO not double-applied

### Step 4: Multiple Time Controls
- [ ] Update matchmaking queue to support time control parameter
- [ ] Add 3+0, 5+0, 10+5 as valid time controls
- [ ] Chess clock: add increment support (add N seconds after each move)
- [ ] Update MATCH_FOUND message to include time control

### Step 5: ELO-Based Matchmaking
- [ ] Update matchmaking to pair by ELO range (±200)
- [ ] Range expansion: ±200 → ±400 after 30s → ±600 after 60s
- [ ] Log when range expands (for debugging)
- [ ] Integration test: verify high/low ELO players eventually get matched if wait is long enough

### Step 6: Game History API
- [ ] `internal/store/game_store.go`: GetGamesByPlayer (paginated)
- [ ] `internal/store/game_store.go`: GetGamePGN (return move list in PGN format)
- [ ] `internal/api/game_handler.go`: GET /users/:id/games, GET /games/:id/pgn
- [ ] Pagination: cursor-based (not offset-based — explain why in ADR)

### Step 7: Documentation
- [ ] ADR: PostgreSQL job queue vs Redis job queue decision
- [ ] ADR: Cursor-based vs offset-based pagination
- [ ] ADR: ELO K-factor choice
- [ ] ARCHITECTURE.md: async job processing flow diagram
- [ ] CLAUDE.md update

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | ELO is updated for both players within 5 seconds of game completion |
| 2 | ELO worker crashes mid-job: on restart, ELO is correctly computed once (not twice) |
| 3 | Game history returns correct games for a player in under 50ms (EXPLAIN ANALYZE verified) |
| 4 | PGN export returns valid PGN that can be imported into Lichess or Chess.com analysis board |
| 5 | Two players with 400+ ELO difference are not immediately matched (ELO-based queue) |
| 6 | All previous phase acceptance criteria still pass |

---

---

