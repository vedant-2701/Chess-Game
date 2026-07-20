package game_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

// --- GetOrHydrate (PHASE_2.md Step 3) ---------------------------------------

func TestGameRegistry_GetOrHydrate_FastPath_AlreadyRegistered(t *testing.T) {
	r := game.NewGameRegistry()
	s := game.NewGameSession("game-1", "user-white")
	r.Register(s)

	var hydrateCalls int32
	got, err := r.GetOrHydrate(context.Background(), "game-1", func(ctx context.Context) (*game.GameSession, error) {
		atomic.AddInt32(&hydrateCalls, 1)
		return nil, errors.New("hydrateFn must not be called on the fast path")
	})
	if err != nil {
		t.Fatalf("GetOrHydrate: %v", err)
	}
	if got != s {
		t.Fatal("expected the already-registered session, got a different pointer")
	}
	if atomic.LoadInt32(&hydrateCalls) != 0 {
		t.Fatalf("expected hydrateFn not to be called on the fast path, called %d times", hydrateCalls)
	}
}

func TestGameRegistry_GetOrHydrate_HydratesOnMiss(t *testing.T) {
	r := game.NewGameRegistry()
	hydrated := game.NewGameSession("game-1", "user-white")

	var hydrateCalls int32
	got, err := r.GetOrHydrate(context.Background(), "game-1", func(ctx context.Context) (*game.GameSession, error) {
		atomic.AddInt32(&hydrateCalls, 1)
		return hydrated, nil
	})
	if err != nil {
		t.Fatalf("GetOrHydrate: %v", err)
	}
	if got != hydrated {
		t.Fatal("expected the hydrated session to be returned")
	}
	if atomic.LoadInt32(&hydrateCalls) != 1 {
		t.Fatalf("expected hydrateFn to be called exactly once, got %d", hydrateCalls)
	}

	// Must also now be registered — a subsequent Get should hit the fast path.
	again, err := r.Get("game-1")
	if err != nil {
		t.Fatalf("Get after hydrate: %v", err)
	}
	if again != hydrated {
		t.Fatal("expected the session to be registered after GetOrHydrate")
	}
}

func TestGameRegistry_GetOrHydrate_HydrateFnError_Propagates(t *testing.T) {
	r := game.NewGameRegistry()
	sentinel := errors.New("db unreachable")

	_, err := r.GetOrHydrate(context.Background(), "game-1", func(ctx context.Context) (*game.GameSession, error) {
		return nil, sentinel
	})
	if err == nil {
		t.Fatal("expected error to propagate from hydrateFn, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel error, got: %v", err)
	}

	// Must not have registered anything on failure.
	if _, getErr := r.Get("game-1"); !errors.Is(getErr, game.ErrGameNotFound) {
		t.Errorf("expected ErrGameNotFound after failed hydration, got: %v", getErr)
	}
}

// TestGameRegistry_GetOrHydrate_ConcurrentMiss_ExactlyOneHydration is
// PHASE_2.md Step 3's explicit regression requirement: two (here, twenty)
// goroutines racing a miss on the same gameID must trigger exactly one
// hydration and all receive the same session pointer.
//
// hydrateFn deliberately blocks on <-release until every goroutine has been
// spawned. This isn't just "nicer" than a fixed delay — it removes the need
// for one entirely (CODING_GUIDELINES.md §6 forbids time.Sleep in tests):
// since hydrateFn cannot return before release is closed, the session cannot
// be registered before release is closed either, which means EVERY caller —
// regardless of how the scheduler happens to interleave them — is
// guaranteed to miss the fast-path Get and race through singleflight for
// real, not merely serialize through it. The result is correct under every
// possible scheduling order, not just a probable one.
func TestGameRegistry_GetOrHydrate_ConcurrentMiss_ExactlyOneHydration(t *testing.T) {
	r := game.NewGameRegistry()
	hydrated := game.NewGameSession("game-1", "user-white")

	var hydrateCalls int32
	release := make(chan struct{})

	const n = 20
	results := make(chan *game.GameSession, n)
	errs := make(chan error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.GetOrHydrate(context.Background(), "game-1", func(ctx context.Context) (*game.GameSession, error) {
				atomic.AddInt32(&hydrateCalls, 1)
				<-release
				return hydrated, nil
			})
			results <- got
			errs <- err
		}()
	}

	// All n goroutines have now been launched (the spawn loop above has
	// returned) — safe to release regardless of how many have actually been
	// scheduled yet, per the doc comment above.
	close(release)

	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrHydrate: %v", err)
		}
	}
	for got := range results {
		if got != hydrated {
			t.Fatal("expected every caller to receive the identical hydrated session pointer")
		}
	}
	if calls := atomic.LoadInt32(&hydrateCalls); calls != 1 {
		t.Fatalf("expected hydrateFn to be called exactly once across %d concurrent callers, got %d", n, calls)
	}
}

// TestGameRegistry_GetOrHydrate_HydrateFnDetachedFromTriggeringCallerCancellation
// is the direct regression test for DECISIONS_LOG_PHASE_2.md ADR-027: the
// context passed to hydrateFn must NOT observe the triggering caller's own
// context being cancelled — otherwise one caller's dropped connection would
// abort hydration for every other caller silently piggybacking on the same
// in-flight singleflight call.
func TestGameRegistry_GetOrHydrate_HydrateFnDetachedFromTriggeringCallerCancellation(t *testing.T) {
	r := game.NewGameRegistry()
	hydrated := game.NewGameSession("game-1", "user-white")

	triggeringCtx, cancelTriggeringCtx := context.WithCancel(context.Background())

	started := make(chan struct{})
	proceed := make(chan struct{})
	var hydrateCtxWasCancelled bool

	type outcome struct {
		session *game.GameSession
		err     error
	}
	done := make(chan outcome, 1)

	go func() {
		got, err := r.GetOrHydrate(triggeringCtx, "game-1", func(hydrateCtx context.Context) (*game.GameSession, error) {
			close(started)
			<-proceed
			// By now the test below has already cancelled triggeringCtx.
			// hydrateCtx must not have observed that cancellation.
			hydrateCtxWasCancelled = hydrateCtx.Err() != nil
			return hydrated, nil
		})
		done <- outcome{session: got, err: err}
	}()

	<-started
	cancelTriggeringCtx() // the triggering caller's own request goes away
	close(proceed)

	result := <-done
	if result.err != nil {
		t.Fatalf("expected hydration to succeed despite the triggering caller's context being cancelled, got: %v", result.err)
	}
	if result.session != hydrated {
		t.Fatal("expected the hydrated session to be returned")
	}
	if hydrateCtxWasCancelled {
		t.Fatal("ADR-027 violation: hydrateFn observed the triggering caller's cancellation — hydration must be detached")
	}
}