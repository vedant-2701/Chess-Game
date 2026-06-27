package game_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vedant-2701/chess/internal/game"
)

func TestGameRegistry_RegisterAndGet(t *testing.T) {
	r := game.NewGameRegistry()
	s := game.NewGameSession("game-1", "user-white")

	r.Register(s)

	got, err := r.Get("game-1")
	if err != nil {
		t.Fatalf("Get returned unexpected error: %v", err)
	}
	if got != s {
		t.Fatal("Get returned a different *GameSession than was registered")
	}
}

func TestGameRegistry_GetMissing(t *testing.T) {
	r := game.NewGameRegistry()

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing gameID, got nil")
	}
	if !errors.Is(err, game.ErrGameNotFound) {
		t.Errorf("expected ErrGameNotFound, got: %v", err)
	}
}

func TestGameRegistry_Unregister(t *testing.T) {
	r := game.NewGameRegistry()
	s := game.NewGameSession("game-1", "user-white")
	r.Register(s)

	r.Unregister("game-1")

	_, err := r.Get("game-1")
	if !errors.Is(err, game.ErrGameNotFound) {
		t.Errorf("expected ErrGameNotFound after Unregister, got: %v", err)
	}
}

func TestGameRegistry_UnregisterMissingIsNoop(t *testing.T) {
	r := game.NewGameRegistry()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Unregister on missing ID must not panic, got: %v", rec)
		}
	}()

	r.Unregister("nobody")
}

func TestGameRegistry_AllActive_ReturnsSnapshot(t *testing.T) {
	r := game.NewGameRegistry()

	// Empty registry must return a non-nil empty slice (not nil — callers
	// range over it and nil serialises to null in JSON).
	all := r.AllActive()
	if all == nil {
		t.Fatal("AllActive must return non-nil slice for empty registry")
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(all))
	}

	s1 := game.NewGameSession("game-1", "user-a")
	s2 := game.NewGameSession("game-2", "user-b")
	r.Register(s1)
	r.Register(s2)

	all = r.AllActive()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
}

func TestGameRegistry_ConcurrentAccess(t *testing.T) {
	r := game.NewGameRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("game-%d", n)
			s := game.NewGameSession(id, "user-white")
			r.Register(s)
			_, _ = r.Get(id)
			r.Unregister(id)
		}(i)
	}

	wg.Wait()
}