package game

import (
	"log/slog"
	"sync"
	"time"

	"github.com/vedant-2701/chess/internal/store"
)

// clockReset is the message sent to the Clock's background goroutine to
// reconfigure the active timeout timer. A zero duration stops the timer
// without switching the active player (used by Pause).
type clockReset struct {
	duration time.Duration
	color    store.Color // the active color at the time of this reset
}

// Clock tracks remaining time for both players in a game and fires a
// registered callback when the active player's time reaches zero.
//
// A single background goroutine is started by Start and terminated by Stop.
// All exported methods are safe for concurrent use.
//
// Boundary race note: there is an inherent race window between a clock timeout
// firing and a concurrent Switch call (e.g. a move submitted at the exact
// instant White's time runs out). This is an accepted Phase 1 limitation
// documented as TD-002. Phase 4 will address it with move submission
// timestamps.
type Clock struct {
	// mu protects: active, whiteRemaining, blackRemaining, startedAt,
	// paused, started, onTimeout.
	mu sync.Mutex

	active         store.Color
	whiteRemaining time.Duration
	blackRemaining time.Duration

	// startedAt is set to time.Now() on every Start and Resume call.
	// Switch and Pause read time.Since(startedAt) to compute elapsed time
	// and deduct it from the active player's remaining duration.
	startedAt time.Time

	paused  bool
	started bool

	onTimeout func(store.Color)

	stopCh  chan struct{}
	resetCh chan clockReset // buffered(1); carries timer reconfigurations to run()
}

// NewClock constructs a Clock with both players initialised to initialMs.
// No background goroutine is started until Start is called.
func NewClock(initialMs int64) *Clock {
	d := time.Duration(initialMs) * time.Millisecond
	return &Clock{
		whiteRemaining: d,
		blackRemaining: d,
		stopCh:         make(chan struct{}),
		resetCh:        make(chan clockReset, 1),
	}
}

// NewClockWithTimes constructs a Clock with independent initial durations per
// player. Used by RestoreActiveGames to hydrate a Clock from persisted DB
// values when the two players' remaining times have diverged from each other.
// No background goroutine is started until Start is called.
func NewClockWithTimes(whiteMs, blackMs int64) *Clock {
	return &Clock{
		whiteRemaining: time.Duration(whiteMs) * time.Millisecond,
		blackRemaining: time.Duration(blackMs) * time.Millisecond,
		stopCh:         make(chan struct{}),
		resetCh:        make(chan clockReset, 1),
	}
}

// IsStarted reports whether Start has been called on this Clock.
// Used by Manager.HandleConnect to choose between Start and Resume when
// both players reconnect after a disconnect or server restart.
func (c *Clock) IsStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// SetTimeoutCallback registers fn to be called when the active player's
// remaining time reaches zero. fn is invoked from the Clock's background
// goroutine and must not call any Clock method (deadlock risk). Must be
// called before Start.
func (c *Clock) SetTimeoutCallback(fn func(store.Color)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTimeout = fn
}

// Start begins counting down for color and launches the background goroutine.
// Calling Start after the first call is a no-op.
func (c *Clock) Start(color store.Color) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.active = color
	c.started = true
	c.startedAt = time.Now()
	remaining := c.remainingFor(color)
	c.mu.Unlock()

	go c.run()
	// resetCh is buffered(1) and empty at Start time — this send cannot block.
	c.resetCh <- clockReset{duration: remaining, color: color}
}

// Switch stops the active player's clock, records elapsed time, and starts
// the opponent's countdown. Called by MoveProcessor after ApplyMove succeeds.
// Switch is a no-op if the clock has not been started or is currently paused.
func (c *Clock) Switch() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started || c.paused {
		return
	}

	c.deductElapsed(time.Since(c.startedAt))

	if c.active == store.ColorWhite {
		c.active = store.ColorBlack
	} else {
		c.active = store.ColorWhite
	}

	c.startedAt = time.Now()
	c.sendReset(c.remainingFor(c.active), c.active)
}

// Pause stops the active player's clock without switching. Called when the
// active player disconnects (TD-002). The active color is preserved so that
// Resume restarts the correct player's countdown.
// Pause is a no-op if the clock has not been started or is already paused.
func (c *Clock) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started || c.paused {
		return
	}

	c.deductElapsed(time.Since(c.startedAt))
	c.paused = true
	// duration 0 tells run() to stop the timer without changing activeColor.
	c.sendReset(0, c.active)
}

// Resume restarts the countdown for the paused active player. If color does
// not match c.active, an Error is logged but c.active is NOT overridden —
// silently reassigning the active color on a caller bug would corrupt clock
// state.
// Resume is a no-op if the clock has not been started or is not paused.
func (c *Clock) Resume(color store.Color) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started || !c.paused {
		return
	}
	if color != c.active {
		slog.Error("Clock.Resume: color does not match active player — resuming active player's clock",
			"requestedColor", color, "activeColor", c.active)
	}
	c.paused = false
	c.startedAt = time.Now()
	c.sendReset(c.remainingFor(c.active), c.active)
}

// TimeRemaining returns the remaining time for color. If the clock is running
// and color is the currently active player, it accounts for elapsed time since
// the last Start or Resume. The returned duration is clamped to zero.
func (c *Clock) TimeRemaining(color store.Color) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started && !c.paused && c.active == color {
		remaining := c.remainingFor(color) - time.Since(c.startedAt)
		if remaining < 0 {
			return 0
		}
		return remaining
	}
	return c.remainingFor(color)
}

// Stop signals the background goroutine to exit and clears the timeout
// callback to prevent a stale callback from firing after Stop returns.
// Safe to call multiple times — subsequent calls are no-ops.
// After Stop, the Clock must not be reused.
func (c *Clock) Stop() {
	c.mu.Lock()
	c.onTimeout = nil // prevent stale callback from firing if timer is in-flight
	select {
	case <-c.stopCh:
		// already closed — double Stop is a no-op
	default:
		close(c.stopCh)
	}
	c.mu.Unlock()
}

// run is the Clock's background goroutine. It owns a time.Timer that it
// reconfigures on each message from resetCh, and exits when stopCh is closed.
// It never acquires c.mu — all shared state is communicated via channels to
// avoid lock ordering issues.
func (c *Clock) run() {
	var timer *time.Timer
	var timerC <-chan time.Time
	var activeColor store.Color

	for {
		select {
		case <-c.stopCh:
			if timer != nil {
				timer.Stop()
				// Drain to unblock any in-flight timer send.
				select {
				case <-timer.C:
				default:
				}
			}
			return

		case msg := <-c.resetCh:
			// Stop and drain the existing timer before replacing it.
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer = nil
				timerC = nil
			}
			activeColor = msg.color
			if msg.duration > 0 {
				timer = time.NewTimer(msg.duration)
				timerC = timer.C
			}
			// duration == 0 means Pause: timerC stays nil so the timer case
			// never fires until the next reset arrives.

		case <-timerC:
			// Active player's time has reached zero.
			c.mu.Lock()
			fn := c.onTimeout
			c.mu.Unlock()
			if fn != nil {
				fn(activeColor)
			}
			timer = nil
			timerC = nil
		}
	}
}

// remainingFor returns the stored remaining time for color.
// Must be called with c.mu held.
func (c *Clock) remainingFor(color store.Color) time.Duration {
	if color == store.ColorWhite {
		return c.whiteRemaining
	}
	return c.blackRemaining
}

// deductElapsed subtracts elapsed from the active player's remaining time,
// clamping to zero. Must be called with c.mu held.
func (c *Clock) deductElapsed(elapsed time.Duration) {
	if c.active == store.ColorWhite {
		c.whiteRemaining -= elapsed
		if c.whiteRemaining < 0 {
			c.whiteRemaining = 0
		}
	} else {
		c.blackRemaining -= elapsed
		if c.blackRemaining < 0 {
			c.blackRemaining = 0
		}
	}
}

// sendReset delivers a new timer configuration to the run goroutine. Any
// unconsumed pending reset is drained first so that the latest configuration
// always wins (e.g. Pause called immediately after Switch replaces the
// Switch reset before the goroutine processes it).
// Must be called with c.mu held.
func (c *Clock) sendReset(d time.Duration, color store.Color) {
	select {
	case <-c.resetCh:
	default:
	}
	c.resetCh <- clockReset{duration: d, color: color}
}
