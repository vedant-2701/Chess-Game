# Chess Server

A production-grade chess platform built in Go, designed as a structured system design learning project. Each phase introduces a new distributed systems concept on top of a working foundation.

This is not a Chess.com clone attempt. It is a deliberate engineering exercise where the architecture evolves phase by phase, with each phase teaching a specific concept in real-time systems, distributed state, and backend infrastructure.

---

## What This Project Teaches

| Phase | Concept |
|-------|---------|
| 1 | WebSocket lifecycle, server-authoritative state, game state machines, session management |
| 2 | WebSocket horizontal scaling, Redis pub/sub, stateful vs stateless services |
| 3 | Queue-based matchmaking, distributed locking, race conditions |
| 4 | Async processing, ELO computation, database indexing strategy |
| 5 | Fan-out write problem, spectator broadcast, connection management at scale |
| 6 | Observability, structured metrics, distributed tracing, operational readiness |

---

## Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.22+ |
| WebSocket | gorilla/websocket |
| HTTP Router | go-chi/chi v5 |
| Chess Logic | notnil/chess |
| Database | PostgreSQL 16 |
| DB Driver | pgx/v5 (pgxpool) |
| Migrations | golang-migrate/migrate v4 |
| Auth | golang-jwt/jwt v5 |
| Cache/PubSub | Redis 7 + go-redis/v9 (Phase 2+) |
| Logging | log/slog (stdlib) |
| Metrics | prometheus/client_golang |
| Dev Infra | Docker + docker-compose |

---

## Prerequisites

- Go 1.22 or higher
- Docker and docker-compose
- `make` (optional but recommended)
- `golang-migrate` CLI for running migrations manually

```bash
# Install golang-migrate CLI
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
```

---

## Local Setup

```bash
# 1. Clone the repository
git clone <repo-url>
cd chess-server

# 2. Copy environment config
cp .env.example .env

# 3. Start infrastructure (PostgreSQL, Redis)
docker compose up -d

# 4. Wait for PostgreSQL to be ready, then run migrations
make migrate-up

# 5. Run the server
make run
```

Server starts on `http://localhost:8080`.

WebSocket endpoint: `ws://localhost:8080/ws/game/:id`

---

## Environment Variables

See `.env.example` for all required variables. Key ones:

```env
DATABASE_URL=postgres://chess:chess@localhost:5432/chess?sslmode=disable
JWT_SECRET=your-secret-key-here
SERVER_PORT=8080
LOG_LEVEL=debug
```

---

## Running Tests

```bash
# All tests
make test

# With race detector (always use this)
make test-race

# Single package
go test -race ./internal/game/...
```

Tests require a running PostgreSQL instance. The test suite uses a separate `chess_test` database, created automatically by `make test-setup`.

---

## Makefile Commands

```bash
make run          # Run the server
make build        # Build binary to ./bin/chess-server
make test         # Run all tests
make test-race    # Run tests with race detector
make migrate-up   # Apply all pending migrations
make migrate-down # Roll back last migration
make lint         # Run golangci-lint
make docker-up    # Start infrastructure containers
make docker-down  # Stop infrastructure containers
```

---

## Project Structure

```
chess-server/
├── cmd/
│   └── server/
│       └── main.go              # Entry point, dependency wiring
├── internal/
│   ├── ws/                      # WebSocket infrastructure layer
│   │   ├── connection.go        # Connection struct, read/write loops
│   │   ├── registry.go          # Connection registry (connID -> *Connection)
│   │   └── handler.go           # HTTP -> WebSocket upgrade handler
│   ├── game/                    # Game application layer
│   │   ├── session.go           # GameSession struct + state machine
│   │   ├── registry.go          # GameRegistry (gameID -> *GameSession)
│   │   ├── manager.go           # Orchestrates ws + game layers
│   │   └── move.go              # Move processing pipeline
│   ├── chess/
│   │   └── validator.go         # Thin wrapper around notnil/chess
│   ├── store/                   # Persistence layer
│   │   ├── postgres.go          # pgxpool setup and connection
│   │   ├── game_store.go        # Game CRUD operations
│   │   └── move_store.go        # Move persistence and history
│   ├── auth/
│   │   └── token.go             # JWT sign/verify for player tokens
│   └── api/                     # HTTP API layer
│       ├── routes.go            # chi router setup
│       └── game_handler.go      # POST /games, POST /games/:id/join
├── migrations/                  # SQL migration files
├── docker-compose.yml
├── Makefile
├── .env.example
└── go.mod
```

---

## Documentation

| Document | Purpose |
|----------|---------|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | How the system is built and why |
| [ROADMAP.md](./ROADMAP.md) | Phase-by-phase build plan and learning objectives |
| [DECISIONS_LOG.md](./DECISIONS_LOG.md) | Every architectural decision with rationale |
| [CODING_GUIDELINES.md](./CODING_GUIDELINES.md) | Code conventions and rules |
| [PHASE_1.md](./PHASE_1.md) | Current phase spec, checklist, and acceptance criteria |
| [CLAUDE.md](./CLAUDE.md) | AI session context — current state and next steps |

---

## Current Status

**Phase 1 — MVP** (In Progress)

See [PHASE_1.md](./PHASE_1.md) for the full checklist and acceptance criteria.
