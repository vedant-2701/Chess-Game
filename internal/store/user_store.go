package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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

// --- PHASE_3.md addendum: username/password login (fixes TD-P3-007) -------
//
// See internal/auth/password.go's doc comment for the full scope rationale.
// These two methods are the store-layer half; internal/api/auth_handler.go
// is the HTTP-layer half.

// CreateUserWithCredentials creates a new user identified by a unique
// username and a pre-computed bcrypt password hash (hashing happens in the
// caller — internal/auth.HashPassword — not here; this method only
// persists, matching this codebase's existing division of responsibility:
// business/crypto logic in the caller, SQL only in the store package).
//
// id is generated here as a UUID v7, not accepted from the caller — unlike
// CreateOrGetUser's client-supplied anonymous ID (migration 001's original
// design: "userID is generated client-side"), a credentialed registration
// has no client-side ID to submit in the first place; the server mints one.
// v7 (time-ordered, B-tree locality), not v4, matching this project's
// stated convention for DB primary keys (games.id uses the same choice).
//
// Returns ErrUsernameTaken if username is already registered — detected via
// ON CONFLICT (username) DO NOTHING RETURNING returning zero rows
// (pgx.ErrNoRows on the Scan), the same "no row means the predicate/conflict
// blocked it" idiom this store already uses elsewhere via RowsAffected(),
// applied here through RETURNING since this is an INSERT, not an UPDATE.
func (s *UserStore) CreateUserWithCredentials(ctx context.Context, username, passwordHash string) (*User, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("UserStore.CreateUserWithCredentials username=%s: generate id: %w", username, err)
	}

	const q = `
		INSERT INTO users (id, username, password_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (username) DO NOTHING
		RETURNING id, created_at`

	var u User
	err = s.pool.QueryRow(ctx, q, id.String(), username, passwordHash).Scan(&u.ID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUsernameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("UserStore.CreateUserWithCredentials username=%s: %w", username, err)
	}
	return &u, nil
}

// GetUserIDAndPasswordHashByUsername looks up credentials for login
// verification. Deliberately does NOT return through the general-purpose
// User struct (which carries no password hash field anywhere else in this
// package, and should not start doing so just for this one caller) — a
// narrowly-scoped positional return keeps the hash from ever being
// reachable through a struct other callers pass around freely.
//
// Returns ErrUserNotFound if no user is registered under that username.
// Callers (AuthHandler.Login) must give this the identical response shape
// as a wrong-password result — this method does not do that itself, since
// distinguishing the two is exactly the username-enumeration side channel
// the caller needs to avoid, and this method's job is only to report what
// is actually true.
func (s *UserStore) GetUserIDAndPasswordHashByUsername(ctx context.Context, username string) (userID, passwordHash string, err error) {
	const q = `SELECT id, password_hash FROM users WHERE username = $1`

	err = s.pool.QueryRow(ctx, q, username).Scan(&userID, &passwordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUserNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("UserStore.GetUserIDAndPasswordHashByUsername username=%s: %w", username, err)
	}
	return userID, passwordHash, nil
}