package game

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/vedant-2701/chess/internal/store"
)

// Run once per call site that needs it, or wrap in a helper:
func verifyNoLeaks(t *testing.T) {
	t.Helper()
	goleak.VerifyNone(t,
		// pgxpool's background health-check goroutine is spawned once by
		// TestMain for the lifetime of the test binary when running with
		// -tags integration. It is not a leak from the code under test —
		// ignoring it by exact stack-top function name keeps real Clock
		// goroutine leaks detectable under both unit and integration runs.
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}

// Note on defer ordering: goleak.VerifyNone(t) must be deferred FIRST so that
// it executes LAST (defers run LIFO). c.Stop() is deferred SECOND so it runs
// FIRST, terminating the goroutine before goleak inspects live goroutines.
//
//	defer goleak.VerifyNone(t) // registered first → executes last
//	defer c.Stop()             // registered second → executes first

// TestClock_InitialTimeRemaining verifies that both players start with the
// configured initial time and that TimeRemaining does not require Start.
func TestClock_InitialTimeRemaining(t *testing.T) {
	c := NewClock(600_000) // 10 minutes

	if got, want := c.TimeRemaining(store.ColorWhite), 600*time.Second; got != want {
		t.Errorf("white initial time = %v, want %v", got, want)
	}
	if got, want := c.TimeRemaining(store.ColorBlack), 600*time.Second; got != want {
		t.Errorf("black initial time = %v, want %v", got, want)
	}
}

// TestClock_CountsDown verifies the active player's time decreases while the
// clock is running. A short initial time (100ms) lets the timeout callback
// fire, confirming the countdown reached zero.
func TestClock_CountsDown(t *testing.T) {
	defer verifyNoLeaks(t)

	done := make(chan struct{}, 1)
	c := NewClock(100)
	c.SetTimeoutCallback(func(_ store.Color) {
		select {
		case done <- struct{}{}:
		default:
		}
	})

	c.Start(store.ColorWhite)
	defer c.Stop()

	select {
	case <-done:
		if remaining := c.TimeRemaining(store.ColorWhite); remaining != 0 {
			t.Errorf("remaining after timeout = %v, want 0", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("clock did not count down to zero within 1s")
	}
}

// TestClock_Switch_ActiveColorChanges verifies that after Switch the opponent
// becomes the active player. After the switch, Black's clock runs until it
// fires the timeout callback with store.ColorBlack.
func TestClock_Switch_ActiveColorChanges(t *testing.T) {
	defer verifyNoLeaks(t)

	timedOut := make(chan store.Color, 1)
	c := NewClock(200)
	c.SetTimeoutCallback(func(color store.Color) {
		select {
		case timedOut <- color:
		default:
		}
	})

	c.Start(store.ColorWhite)
	c.Switch() // White elapsed ≈ 0; Black now active with ≈ 200ms remaining
	defer c.Stop()

	select {
	case color := <-timedOut:
		if color != store.ColorBlack {
			t.Errorf("timeout fired for %s, want %s", color, store.ColorBlack)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout callback did not fire within 1s after Switch")
	}
}

// TestClock_PauseResume_PreservesRemainingTime verifies that time is not
// consumed while the clock is paused, and that Resume correctly restarts
// the countdown without losing the remaining duration.
func TestClock_PauseResume_PreservesRemainingTime(t *testing.T) {
	defer verifyNoLeaks(t)

	done := make(chan store.Color, 1)
	// 150ms per player: short enough for a fast test, long enough that Pause
	// is called before expiry even under modest scheduling jitter.
	c := NewClock(150)
	c.SetTimeoutCallback(func(color store.Color) {
		select {
		case done <- color:
		default:
		}
	})

	c.Start(store.ColorWhite)
	c.Pause()
	defer c.Stop()

	// Verify timeout does NOT fire while paused. 300ms > 2x the initial time,
	// so if the clock were still running it would have fired by now.
	select {
	case color := <-done:
		t.Fatalf("timeout fired for %s while clock was paused", color)
	case <-time.After(300 * time.Millisecond):
		// expected: no timeout while paused
	}

	// Resume: remaining is approximately 150ms (negligible elapsed before Pause).
	c.Resume(store.ColorWhite)

	select {
	case color := <-done:
		if color != store.ColorWhite {
			t.Errorf("timeout fired for %s after Resume, want %s", color, store.ColorWhite)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout did not fire after Resume within 1s")
	}
}

// TestClock_Stop_TerminatesGoroutine verifies that Stop cleanly exits the
// background goroutine. goleak.VerifyNone detects any surviving goroutine.
func TestClock_Stop_TerminatesGoroutine(t *testing.T) {
	defer verifyNoLeaks(t)

	c := NewClock(60_000) // 60 seconds — will not expire during test
	c.SetTimeoutCallback(func(_ store.Color) {})
	c.Start(store.ColorWhite)
	c.Stop()
	// No defer c.Stop() needed — already stopped above.
	// goleak runs after this function returns and must find no leaked goroutine.
}

// TestClock_Stop_Idempotent verifies that calling Stop more than once does not
// panic (guard against double close of stopCh).
func TestClock_Stop_Idempotent(t *testing.T) {
	defer verifyNoLeaks(t)

	c := NewClock(60_000)
	c.Start(store.ColorWhite)
	c.Stop()
	c.Stop() // must not panic
}

// TestClock_Start_Idempotent verifies that calling Start more than once does
// not start a second goroutine or change the active player.
func TestClock_Start_Idempotent(t *testing.T) {
	defer verifyNoLeaks(t)

	c := NewClock(60_000)
	c.Start(store.ColorWhite)
	c.Start(store.ColorBlack) // second call must be a no-op
	defer c.Stop()

	// Active should still be White from the first Start.
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active != store.ColorWhite {
		t.Errorf("active after double Start = %s, want %s", active, store.ColorWhite)
	}
}

// TestClock_Resume_WrongColor_LogsErrorAndResumesActive verifies that Resume
// with a color that does not match the active player does not override c.active.
// The timeout must still fire for White (the actual active player).
func TestClock_Resume_WrongColor_LogsErrorAndResumesActive(t *testing.T) {
	defer verifyNoLeaks(t)

	done := make(chan store.Color, 1)
	c := NewClock(100)
	c.SetTimeoutCallback(func(color store.Color) {
		select {
		case done <- color:
		default:
		}
	})

	c.Start(store.ColorWhite)
	c.Pause()
	// Resume with wrong color. The implementation logs an error and resumes
	// the actual active player (White) — c.active must not be overridden.
	c.Resume(store.ColorBlack)
	defer c.Stop()

	select {
	case color := <-done:
		if color != store.ColorWhite {
			t.Errorf("timeout fired for %s, want %s — active color was corrupted", color, store.ColorWhite)
		}
	case <-time.After(time.Second):
		t.Fatal("clock did not time out within 1s after Resume with wrong color")
	}
}

// TestClock_ConcurrentTimeRemaining_RaceFree verifies that concurrent reads of
// TimeRemaining from multiple goroutines do not produce data races.
// The race detector catches violations when run with go test -race.
func TestClock_ConcurrentTimeRemaining_RaceFree(t *testing.T) {
	defer verifyNoLeaks(t)

	c := NewClock(60_000)
	c.Start(store.ColorWhite)
	defer c.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.TimeRemaining(store.ColorWhite)
			_ = c.TimeRemaining(store.ColorBlack)
		}()
	}
	wg.Wait()
}
