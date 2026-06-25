# Phase 6 — Observability

**Status: ⬜ Not Started**
**Prerequisite: Phase 5 all acceptance criteria met**

---

## Objective

Make the system understandable in production. Add structured logging, Prometheus metrics, health checks, and a runbook. This phase does not add features. It makes the existing system operable.

**The single most important learning outcome of Phase 6:**
A system you cannot measure is a system you cannot trust. The gap between a student project and a production system is not features — it is observability. You cannot debug a production issue by reading code. You debug it by reading metrics, logs, and traces.

---

## The Problem This Phase Solves

Right now, if move latency degrades in production, you would not know until players complain. If a goroutine is leaking, you would not know until the server runs out of memory. If the ELO worker is falling behind, you would not know until ratings are hours out of date.

Observability means: the system tells you about its own health without you having to ask.

---

## Scope

### In Scope

- Prometheus metrics exposed at `GET /metrics`
- Structured request logging with request IDs propagated through all layers
- Liveness and readiness health check endpoints
- Graceful shutdown with configurable drain timeout
- pprof endpoint for production profiling (behind auth)
- A written runbook: what to do when each alert fires

### Explicitly Out of Scope

| Feature | Why |
|---------|-----|
| Grafana dashboards | Visualization layer, not backend work |
| Distributed tracing (Jaeger/Zipkin) | Requires more infrastructure than warranted at this stage |
| Log aggregation (ELK/Loki) | Infrastructure concern, not application concern |
| Alerting rules | Depends on Prometheus + Alertmanager deployment |
| SLO/SLA definition | Post-learning-project concern |

---

## Metrics to Implement

Every metric below has a specific reason it exists. Do not add metrics for the sake of having metrics.

### WebSocket Metrics
```
chess_ws_connections_active          gauge   — Active WebSocket connections right now
chess_ws_connections_total           counter — Total connections since startup (for rate calculation)
chess_ws_messages_received_total     counter — by type (MOVE, RESIGN, PING, etc.)
chess_ws_messages_sent_total         counter — by type
```

**Why:** Active connections tells you if you have a leak. Message rate tells you if traffic is normal.

### Game Metrics
```
chess_games_active                   gauge   — Games currently in ACTIVE state
chess_games_total                    counter — by outcome (CHECKMATE, STALEMATE, TIMEOUT, RESIGNATION, ABANDONED)
chess_game_duration_seconds          histogram — from ACTIVE to COMPLETED
chess_moves_total                    counter — total moves processed
```

**Why:** Game duration histogram tells you if something is wrong with clock behavior. ABANDONED rate tells you about player experience.

### Move Pipeline Metrics
```
chess_move_processing_duration_ms    histogram — full pipeline: validate → persist → broadcast
chess_move_db_write_duration_ms      histogram — just the DB write
chess_move_rejected_total            counter  — by reason (illegal_move, wrong_turn, game_not_active)
```

**Why:** This is the most critical latency in the system. p99 above 200ms is a problem. p99 above 500ms is player-noticeable.

### ELO Worker Metrics
```
chess_elo_jobs_pending               gauge   — Jobs waiting to be processed
chess_elo_jobs_processed_total       counter — by status (completed, failed)
chess_elo_processing_duration_ms     histogram — time to process one job
chess_elo_queue_lag_seconds          gauge   — age of oldest pending job
```

**Why:** Queue lag tells you if the worker is keeping up. A growing lag means the worker is falling behind.

### Matchmaking Metrics
```
chess_matchmaking_queue_depth        gauge   — by time control
chess_matchmaking_wait_duration_ms   histogram — time from queue entry to match
```

---

## Health Check Design

Two distinct endpoints. The distinction matters.

### `GET /health/live` — Liveness
"Is this process alive and able to handle requests?"

Fails if:
- Process is deadlocked
- Memory usage is critically high

Returns 200 if the process is running. Returns 503 if it should be killed and restarted.

### `GET /health/ready` — Readiness
"Is this process ready to receive traffic?"

Fails if:
- PostgreSQL connection pool is exhausted or unreachable
- Redis is unreachable (Phase 2+)
- Pending migrations exist (on startup)

Returns 200 if traffic should be sent to this instance. Returns 503 if the load balancer should stop sending traffic to this instance (but not kill it).

**Why this distinction matters:** If you only have one health endpoint and it checks DB connectivity, a slow DB causes the load balancer to kill your server and restart it — which doesn't fix the slow DB and now you have a restart loop. Readiness failing removes the instance from rotation. Liveness failing restarts the process.

---

## Request ID Propagation

Every incoming request gets a unique request ID. This ID propagates through all log lines for that request, including across goroutines.

```go
// Middleware generates request ID
func RequestID(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        requestID := r.Header.Get("X-Request-ID")
        if requestID == "" {
            requestID = uuid.New().String()
        }
        ctx := context.WithValue(r.Context(), RequestIDKey, requestID)
        w.Header().Set("X-Request-ID", requestID)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

// Every log call extracts request ID from context
slog.InfoContext(ctx, "move processed",
    "requestID", RequestIDFromContext(ctx),
    "gameID", gameID,
    "san", san,
    "durationMs", elapsed.Milliseconds(),
)
```

---

## Runbook

A runbook is a document that tells an on-call engineer what to do when an alert fires. Writing one forces you to think about failure modes.

Runbook entries to write for Phase 6:

| Alert | Symptom | Likely Cause | Action |
|-------|---------|--------------|--------|
| `chess_ws_connections_active` spiking | Sudden connection spike | Load spike or connection leak | Check connection creation rate vs close rate; check for goroutine leak via pprof |
| `chess_move_processing_duration_ms` p99 > 500ms | Slow move delivery | DB write latency or lock contention | Check `chess_move_db_write_duration_ms`; run `pg_stat_activity` |
| `chess_elo_queue_lag_seconds` > 60s | ELO updates falling behind | Worker crashed or DB slow | Check ELO worker logs; check `elo_jobs` table for PROCESSING entries stuck >5 min |
| `/health/ready` returning 503 | Instance removed from rotation | DB unreachable | Check PostgreSQL; check connection pool exhaustion |

---

## Implementation Checklist

### Step 1: Request ID Middleware
- [ ] `internal/api/middleware.go`: RequestID middleware
- [ ] Propagate request ID through context
- [ ] Helper: `RequestIDFromContext(ctx) string`
- [ ] All slog calls in request handlers use `slog.InfoContext(ctx, ...)` not `slog.Info(...)`

### Step 2: Prometheus Metrics
- [ ] `internal/metrics/metrics.go`: define all metrics with correct types (counter/gauge/histogram)
- [ ] Register metrics in a custom registry (not default global — avoids conflicts in tests)
- [ ] Instrument move pipeline: timing from receive to broadcast
- [ ] Instrument DB writes: timing for each store method
- [ ] Instrument WebSocket: active connections gauge (increment on connect, decrement on disconnect)
- [ ] Instrument game lifecycle: games_active gauge, games_total counter
- [ ] Instrument ELO worker: queue depth, processing duration, queue lag
- [ ] Expose `GET /metrics` with Prometheus text format

### Step 3: Health Checks
- [ ] `GET /health/live`: always 200 if process is running
- [ ] `GET /health/ready`: checks DB pool (ping), Redis ping (Phase 2+)
- [ ] Update docker-compose healthcheck to use `/health/live`
- [ ] Update nginx to use `/health/ready` for upstream health checks

### Step 4: pprof
- [ ] Add `net/http/pprof` endpoint at `/debug/pprof/`
- [ ] Require a secret header or basic auth (do not expose publicly)
- [ ] Verify: `go tool pprof http://localhost:8080/debug/pprof/goroutine` shows goroutine count

### Step 5: Graceful Shutdown Hardening
- [ ] Configurable drain timeout via environment variable (default: 30s)
- [ ] On SIGTERM: stop accepting new games, stop matchmaking loop, wait for in-flight moves
- [ ] Log each stage of shutdown with timing
- [ ] Verify: sending SIGTERM during a move completes the move before shutdown

### Step 6: Runbook
- [ ] Create `RUNBOOK.md` in repository root
- [ ] Document each metric and what it means
- [ ] Document each alert scenario with diagnosis steps
- [ ] Document: how to read pprof output, what goroutine leak looks like

### Step 7: Documentation
- [ ] ARCHITECTURE.md: observability section (metrics, health checks, logging)
- [ ] ADR: Prometheus chosen over other metrics systems
- [ ] ADR: two-endpoint health check design
- [ ] CLAUDE.md update (project is now production-grade)

---

## Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | `GET /metrics` returns valid Prometheus text format with all defined metrics |
| 2 | Move processing p99 latency is measurable and visible in metrics |
| 3 | Every error log contains requestID, gameID (when available), and error context |
| 4 | `GET /health/ready` returns 503 when PostgreSQL is unreachable |
| 5 | `GET /health/live` returns 200 even when PostgreSQL is unreachable |
| 6 | Goroutine count after 10 completed games is the same as after 0 games (no leak) |
| 7 | Server handles SIGTERM during a move: move completes, then server shuts down cleanly |
| 8 | RUNBOOK.md exists and has entries for all defined alert scenarios |
| 9 | All previous phase acceptance criteria still pass |

---

## What "Production-Grade" Means at Phase 6 Completion

After Phase 6, this project demonstrates:

- Server-authoritative real-time game state with reconnection
- Horizontal scaling via Redis pub/sub
- Distributed-safe matchmaking with atomic queue operations
- Async job processing with idempotency
- Fan-out broadcast without blocking the critical path
- Structured observability: metrics, logs, health checks

That is a legitimate distributed systems portfolio project. Not Chess.com. But a demonstration that you understand the concepts, made deliberate tradeoffs, and can explain every decision.
