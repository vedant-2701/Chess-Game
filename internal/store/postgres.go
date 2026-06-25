package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates and verifies a PostgreSQL connection pool.
//
// databaseURL accepts the postgres:// scheme (used by the migrate CLI in the
// Makefile) and the pgx5:// scheme (used when wiring golang-migrate into
// main.go at Step 13). Both point to the same database — the scheme difference
// selects the driver, not the host.
//
// The pool is pinged before returning to catch misconfigured URLs at startup
// rather than at first query.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store.NewPool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store.NewPool ping: %w", err)
	}
	return pool, nil
}