//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
)

func TestUserStore_CreateOrGetUser(t *testing.T) {
	us := newUserStore()
	ctx := context.Background()

	t.Run("creates new user and returns it", func(t *testing.T) {
		truncateAll(t)

		const id = "00000000-0000-0000-0000-000000000001"
		user, err := us.CreateOrGetUser(ctx, id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user.ID != id {
			t.Errorf("ID: got %q, want %q", user.ID, id)
		}
		if user.CreatedAt.IsZero() {
			t.Error("CreatedAt should be non-zero")
		}
	})

	t.Run("returns existing user on second call (idempotent)", func(t *testing.T) {
		truncateAll(t)

		const id = "00000000-0000-0000-0000-000000000002"
		first, err := us.CreateOrGetUser(ctx, id)
		if err != nil {
			t.Fatalf("first call: %v", err)
		}

		second, err := us.CreateOrGetUser(ctx, id)
		if err != nil {
			t.Fatalf("second call: %v", err)
		}

		if first.ID != second.ID {
			t.Errorf("ID mismatch: first=%q second=%q", first.ID, second.ID)
		}
		// created_at must not change on the second call
		if !first.CreatedAt.Equal(second.CreatedAt) {
			t.Errorf("CreatedAt changed: first=%v second=%v", first.CreatedAt, second.CreatedAt)
		}
	})

	t.Run("multiple distinct users do not collide", func(t *testing.T) {
		truncateAll(t)

		ids := []string{
			"00000000-0000-0000-0000-000000000003",
			"00000000-0000-0000-0000-000000000004",
		}
		for _, id := range ids {
			if _, err := us.CreateOrGetUser(ctx, id); err != nil {
				t.Fatalf("CreateOrGetUser id=%s: %v", id, err)
			}
		}

		u, err := us.GetUser(ctx, ids[0])
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if u.ID != ids[0] {
			t.Errorf("GetUser returned wrong user: got %q, want %q", u.ID, ids[0])
		}
	})
}

func TestUserStore_GetUser(t *testing.T) {
	us := newUserStore()
	ctx := context.Background()

	t.Run("returns user that exists", func(t *testing.T) {
		truncateAll(t)

		const id = "00000000-0000-0000-0000-000000000010"
		mustCreateUser(t, id)

		user, err := us.GetUser(ctx, id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user.ID != id {
			t.Errorf("ID: got %q, want %q", user.ID, id)
		}
	})

	t.Run("returns ErrUserNotFound for unknown ID", func(t *testing.T) {
		truncateAll(t)

		_, err := us.GetUser(ctx, "00000000-0000-0000-0000-000000000099")
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got: %v", err)
		}
	})
}
