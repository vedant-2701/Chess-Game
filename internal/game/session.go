package game

import (
	"fmt"
	"log/slog"
	"sync"

	notnil "github.com/notnil/chess"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// InitialTimeMs is the starting clock for each player: 10 minutes in milliseconds.
// Phase 1 supports a single time control only (10+0). See PHASE_1.md scope.
const InitialTimeMs int64 = 600_000

// validTransitions defines the legal state machine edges.
// COMPLETED and ABANDONED are terminal — they have no outgoing edges.
var validTransitions = map[store.GameStatus]map[store.GameStatus]bool{
	store.GameStatusWaiting: {
		store.GameStatusActive: true,
	},
	store.GameStatusActive: {
		store.GameStatusCompleted: true,
		store.GameStatusAbandoned: true,
	},
}

// GameStateSnapshot is a point-in-time read of a GameSession's full state.
// Used to construct GAME_STATE WebSocket messages and for reconnection state delivery.
type GameStateSnapshot struct {
	ID            string
	Status        store.GameStatus
	PlayerWhiteID string
	PlayerBlackID string // empty string when no black player has joined
	CurrentFEN    string
	Turn          store.Color
	Moves         []string // annotated SAN from chess.MoveHistory (e.g. "Qxf7#")
	WhiteTimeMs   int64
	BlackTimeMs   int64
	Outcome       *store.Outcome
	OutcomeReason *store.OutcomeReason
}

// GameSession holds all runtime state for a single in-progress game.
// It is the single authoritative in-memory source of truth for a game's state.
type GameSession struct {
	ID string

	// mu protects: status, playerWhiteID, playerBlackID, playerWhite, playerBlack.
	mu            sync.RWMutex
	status        store.GameStatus
	playerWhiteID string
	playerBlackID string         // empty string until SetPlayerBlack is called
	playerWhite   *ws.Connection // nil when White is disconnected
	playerBlack   *ws.Connection // nil when Black is disconnected

	// mu also protects: board, whiteTimeMs, blackTimeMs, outcome, outcomeReason.
	board         *notnil.Game
	whiteTimeMs   int64
	blackTimeMs   int64
	outcome       *store.Outcome       // nil until game reaches a terminal state
	outcomeReason *store.OutcomeReason // nil until game reaches a terminal state

	// clock manages per-player countdown timers. Its internal state is
	// protected by clock.mu (not session.mu). The pointer is set once in
	// NewGameSession and never replaced.
	clock *Clock
}

// NewGameSession constructs a GameSession for a newly created game. The board
// is at the standard starting position and the clock is initialised to InitialTimeMs
// for both players. Status is WAITING_FOR_PLAYER until the second player connects.
func NewGameSession(id string, whiteID string) *GameSession {
	return &GameSession{
		ID:            id,
		status:        store.GameStatusWaiting,
		playerWhiteID: whiteID,
		board:         internalchess.NewGame(),
		whiteTimeMs:   InitialTimeMs,
		blackTimeMs:   InitialTimeMs,
		clock:         NewClock(InitialTimeMs),
	}
}

// NewGameSessionFromDB hydrates a GameSession from an existing database record
// and a board reconstructed by replaying stored moves. Used exclusively by
// Manager.RestoreActiveGames on server startup.
//
// The clock is initialised with the persisted remaining times for each player
// and is NOT started — it starts when both players reconnect via HandleConnect.
func NewGameSessionFromDB(game *store.Game, board *notnil.Game) *GameSession {
	s := &GameSession{
		ID:            game.ID,
		status:        game.Status,
		playerWhiteID: game.PlayerWhiteID,
		board:         board,
		whiteTimeMs:   game.WhiteTimeMs,
		blackTimeMs:   game.BlackTimeMs,
		clock:         NewClockWithTimes(game.WhiteTimeMs, game.BlackTimeMs),
	}
	if game.PlayerBlackID != nil {
		s.playerBlackID = *game.PlayerBlackID
	}
	if game.Outcome != nil {
		s.outcome = game.Outcome
	}
	if game.OutcomeReason != nil {
		s.outcomeReason = game.OutcomeReason
	}
	return s
}

// SetPlayerBlack records the userID of the player who joined as Black.
// Called by Manager.JoinGame after the database record is updated.
func (s *GameSession) SetPlayerBlack(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playerBlackID = userID
}

// RegisterConnection sets the *ws.Connection for the given color. If this registration
// completes the player pair and the game is in WAITING_FOR_PLAYER status, it atomically
// transitions the game to ACTIVE and returns activated=true. Returns ErrConnectionOccupied
// if the slot is already held by a live connection. Use ReplaceConnection for reconnection flows.
func (s *GameSession) RegisterConnection(color store.Color, conn *ws.Connection) (activated bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch color {
	case store.ColorWhite:
		if s.playerWhite != nil {
			return false, fmt.Errorf("GameSession.RegisterConnection color=%s: %w", color, ErrConnectionOccupied)
		}
		s.playerWhite = conn
	case store.ColorBlack:
		if s.playerBlack != nil {
			return false, fmt.Errorf("GameSession.RegisterConnection color=%s: %w", color, ErrConnectionOccupied)
		}
		s.playerBlack = conn
	}

	if s.playerWhite != nil && s.playerBlack != nil && s.status == store.GameStatusWaiting {
		// Route through transitionLocked rather than assigning s.status
		// directly, so validTransitions remains the single source of truth
		// for legal edges (ADR-017 follow-up).
		if tErr := s.transitionLocked(store.GameStatusActive); tErr != nil {
			// Unreachable in practice: s.mu has been held continuously since
			// the status == WAITING check above, and WAITING→ACTIVE is a
			// valid edge. Treated as an invariant violation, not swallowed.
			return false, fmt.Errorf("GameSession.RegisterConnection gameID=%s: %w", s.ID, tErr)
		}
		return true, nil
	}

	return false, nil
}

// ReplaceConnection unconditionally replaces the *ws.Connection for the given
// color. Used during reconnection when the old connection pointer is stale.
func (s *GameSession) ReplaceConnection(color store.Color, conn *ws.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch color {
	case store.ColorWhite:
		s.playerWhite = conn
	case store.ColorBlack:
		s.playerBlack = conn
	}
}

// ClearConnection sets the connection pointer for the given color to nil.
// Called when a player's WebSocket connection drops.
func (s *GameSession) ClearConnection(color store.Color) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch color {
	case store.ColorWhite:
		s.playerWhite = nil
	case store.ColorBlack:
		s.playerBlack = nil
	}
}

// BothPlayersConnected reports whether both players currently have live
// WebSocket connections. Used to determine when to transition WAITING → ACTIVE.
func (s *GameSession) BothPlayersConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.playerWhite != nil && s.playerBlack != nil
}

// IsPlayerConnected reports whether the given color currently has a live
// WebSocket connection. Used by Manager.onAbandonTimeout to distinguish a
// single-player disconnect (opponent wins by abandonment) from a both-players-
// disconnected scenario (abandonment draw) per PHASE_1.md's state machine.
func (s *GameSession) IsPlayerConnected(color store.Color) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch color {
	case store.ColorWhite:
		return s.playerWhite != nil
	case store.ColorBlack:
		return s.playerBlack != nil
	}
	return false
}

// transitionLocked validates newStatus against validTransitions and, if
// legal, applies it. Callers must already hold s.mu (write lock) — this
// method does not lock. This is the single point where validTransitions is
// consulted; both Transition (external callers) and RegisterConnection's
// WAITING→ACTIVE fast path (ADR-017) route through it so there is exactly
// one source of truth for which state-machine edges are legal.
func (s *GameSession) transitionLocked(newStatus store.GameStatus) error {
	allowed, ok := validTransitions[s.status]
	if !ok || !allowed[newStatus] {
		return fmt.Errorf("GameSession.transitionLocked gameID=%s %s→%s: %w",
			s.ID, s.status, newStatus, ErrInvalidTransition)
	}
	s.status = newStatus
	return nil
}

// Transition advances the game state machine to newStatus. Returns
// ErrInvalidTransition for any edge not defined in validTransitions.
// This is the only place game status changes; no external code sets
// status directly.
func (s *GameSession) Transition(newStatus store.GameStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked(newStatus)
}

// SetOutcome records the game result. Must be called after Transition(COMPLETED)
// or Transition(ABANDONED). The move pipeline and manager are responsible for
// calling these in the correct order.
func (s *GameSession) SetOutcome(outcome store.Outcome, reason store.OutcomeReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcome = &outcome
	s.outcomeReason = &reason
}

// UpdateClocks updates the persisted clock state for both players. Called by
// the Clock (Step 9) after each move and on disconnect.
func (s *GameSession) UpdateClocks(whiteMs, blackMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.whiteTimeMs = whiteMs
	s.blackTimeMs = blackMs
}

// CurrentStateSnapshot returns a consistent point-in-time read of the full
// session state. All fields are read under the session read lock to prevent
// torn reads. Used to construct GAME_STATE WebSocket messages.
func (s *GameSession) CurrentStateSnapshot() GameStateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return GameStateSnapshot{
		ID:            s.ID,
		Status:        s.status,
		PlayerWhiteID: s.playerWhiteID,
		PlayerBlackID: s.playerBlackID,
		CurrentFEN:    internalchess.CurrentFEN(s.board),
		Turn:          boardTurn(s.board),
		Moves:         internalchess.MoveHistory(s.board),
		WhiteTimeMs:   s.whiteTimeMs,
		BlackTimeMs:   s.blackTimeMs,
		Outcome:       s.outcome,
		OutcomeReason: s.outcomeReason,
	}
}

// SendToPlayer sends msg to the player of the given color if they have an
// active connection. Returns an error if the player is not connected or
// if the connection's outbound queue is full.
func (s *GameSession) SendToPlayer(color store.Color, msg []byte) error {
	s.mu.RLock()
	var conn *ws.Connection
	switch color {
	case store.ColorWhite:
		conn = s.playerWhite
	case store.ColorBlack:
		conn = s.playerBlack
	}
	s.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("GameSession.SendToPlayer gameID=%s color=%s: player not connected", s.ID, color)
	}
	return conn.Send(msg)
}

// SendToBothPlayers sends msg to both players. Per-player failures are logged
// at Warn level but do not abort delivery to the other player — a disconnected
// or slow client must never block message delivery to their opponent.
func (s *GameSession) SendToBothPlayers(msg []byte) {
	s.mu.RLock()
	white := s.playerWhite
	black := s.playerBlack
	s.mu.RUnlock()

	if white != nil {
		if err := white.Send(msg); err != nil {
			slog.Warn("failed to send to white player", "gameID", s.ID, "error", err)
		}
	}
	if black != nil {
		if err := black.Send(msg); err != nil {
			slog.Warn("failed to send to black player", "gameID", s.ID, "error", err)
		}
	}
}

// CloseConnections sends a WebSocket close frame to both players and clears
// their connection slots.
//
// Callers: this must only be called immediately after, and from the same
// goroutine as, the call that sent the terminal GAME_OVER message —
// Manager.startEventSubscriber's GAME_OVER branch and Manager.publishGameOver's
// EventBus-failure fallback branch are the two (and only) correct call sites.
// It must never be called from Manager.finalizeGame: finalizeGame runs on a
// different goroutine than whichever goroutine actually sent GAME_OVER, and
// LocalEventBus.Publish's buffered channel send only guarantees the event was
// enqueued for the subscriber goroutine to eventually pick up — not that the
// subscriber has run yet (see eventbus.go). Closing from finalizeGame would
// race the close frame against GAME_OVER's delivery through this session's
// shared per-connection outbound queue, with a real chance of the close frame
// winning and the client never receiving GAME_OVER at all. Calling this from
// the same goroutine that just sent GAME_OVER, immediately after, relies on
// nothing but Go's program-order guarantee within a single goroutine plus the
// outbound queue's FIFO draining by that connection's single WriteLoop —
// both already-relied-upon guarantees elsewhere in this codebase, not a new
// assumption.
//
// Without this at all, a client that keeps sending messages after its game
// ends gets silent non-responses instead of a real protocol-level close:
// Manager.HandleMessage's registry lookup fails once finalizeGame has
// unregistered the session, and the resulting error is only logged, never
// reported back to the client. Found via manual E2E testing (PHASE_1.md
// Step 14), not by any automated test — flagged as a real coverage gap.
func (s *GameSession) CloseConnections(statusCode int, reason string) {
	s.mu.Lock()
	white, black := s.playerWhite, s.playerBlack
	s.playerWhite, s.playerBlack = nil, nil
	s.mu.Unlock()

	if white != nil {
		if err := white.SendCloseFrame(statusCode, reason); err != nil {
			slog.Warn("GameSession.CloseConnections: failed to close white connection",
				"gameID", s.ID, "error", err)
		}
	}
	if black != nil {
		if err := black.SendCloseFrame(statusCode, reason); err != nil {
			slog.Warn("GameSession.CloseConnections: failed to close black connection",
				"gameID", s.ID, "error", err)
		}
	}
}

// boardTurn maps the notnil/chess position turn to a store.Color.
// Called under the session read lock — g must not be mutated concurrently.
func boardTurn(g *notnil.Game) store.Color {
	if g.Position().Turn() == notnil.White {
		return store.ColorWhite
	}
	return store.ColorBlack
}