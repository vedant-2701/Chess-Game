package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserStore handles persistence for the users table.
type UserStore struct {
	pool *pgxpool.Pool
}

// NewUserStore constructs a UserStore backed by the given pool.
func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// CreateOrGetUser upserts a user record by ID.
//
// If the userID does not exist, a new row is inserted and returned.
// If the userID already exists, the existing row is returned unchanged.
// This is idempotent: calling it multiple times with the same ID is safe.
//
// The upsert uses ON CONFLICT DO UPDATE with a no-op assignment to force
// RETURNING to fire on conflict, avoiding a separate SELECT round-trip.
func (s *UserStore) CreateOrGetUser(ctx context.Context, userID string) (*User, error) {
	const q = `
		INSERT INTO users (id)
		VALUES ($1)
		ON CONFLICT (id) DO UPDATE SET created_at = users.created_at
		RETURNING id, created_at`

	var u User
	err := s.pool.QueryRow(ctx, q, userID).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("UserStore.CreateOrGetUser id=%s: %w", userID, err)
	}
	return &u, nil
}

// GetUser returns the user with the given ID.
// Returns ErrUserNotFound if no user with that ID exists.
func (s *UserStore) GetUser(ctx context.Context, id string) (*User, error) {
	const q = `SELECT id, created_at FROM users WHERE id = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, id).Scan(&u.ID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("UserStore.GetUser id=%s: %w", id, err)
	}
	return &u, nil
}