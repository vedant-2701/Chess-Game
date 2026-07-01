package game

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

const abandonTimeout = 60 * time.Second

// clientMsg is the minimal envelope parsed from every raw WebSocket message.
// The Type field routes to the appropriate handler; SAN is only present for
// MOVE messages.
type clientMsg struct {
	Type string `json:"type"`
	SAN  string `json:"san,omitempty"`
}

// gameStateMsg is the JSON payload for GAME_STATE WebSocket messages.
type gameStateMsg struct {
	Type          string   `json:"type"`
	FEN           string   `json:"fen"`
	Turn          string   `json:"turn"`
	Moves         []string `json:"moves"`
	Status        string   `json:"status"`
	WhiteTimeMs   int64    `json:"whiteTimeMs"`
	BlackTimeMs   int64    `json:"blackTimeMs"`
	Outcome       *string  `json:"outcome"`
	OutcomeReason *string  `json:"outcomeReason"`
}

// moveRejectedMsg is the JSON payload for MOVE_REJECTED WebSocket messages.
type moveRejectedMsg struct {
	Type   string `json:"type"`
	SAN    string `json:"san"`
	Reason string `json:"reason"`
}

// errMsg is the JSON payload for ERROR WebSocket messages.
type errMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// simpleMsg is used for messages that carry only a type field
// (PONG, OPPONENT_CONNECTED, OPPONENT_DISCONNECTED, OPPONENT_RECONNECTED).
type simpleMsg struct {
	Type string `json:"type"`
}

// Manager is the top-level orchestrator for the game application layer.
// It owns the full lifecycle of game sessions: creation, player join, WebSocket
// connection handling, message routing, reconnection, clock start/resume,
// abandonment detection, and server-restart recovery.
//
// Manager has no knowledge of WebSocket framing — it receives decoded messages
// and acts on GameSession and MoveProcessor. All persistence goes through the
// store layer.
type Manager struct {
	registry  *GameRegistry
	processor *MoveProcessor
	gameStore *store.GameStore
	moveStore *store.MoveStore
	eventBus  EventBus
	jwtSecret string
	validator *internalchess.Validator

	// mu protects: abandonTimers.
	mu            sync.Mutex
	abandonTimers map[string]*time.Timer // key: gameID+":"+string(color)
}

// NewManager constructs a Manager with all required dependencies.
func NewManager(
	registry *GameRegistry,
	processor *MoveProcessor,
	gameStore *store.GameStore,
	moveStore *store.MoveStore,
	eventBus EventBus,
	jwtSecret string,
	validator *internalchess.Validator,
) *Manager {
	return &Manager{
		registry:      registry,
		processor:     processor,
		gameStore:     gameStore,
		moveStore:     moveStore,
		eventBus:      eventBus,
		jwtSecret:     jwtSecret,
		validator:     validator,
		abandonTimers: make(map[string]*time.Timer),
	}
}

// CreateGame creates a new game for userID as White, persists it, registers it
// in the GameRegistry, and returns the session and White's signed player token.
//
// The caller (API handler) is responsible for ensuring the user record exists
// via store.UserStore.CreateOrGetUser before calling CreateGame.
func (m *Manager) CreateGame(ctx context.Context, userID string) (*GameSession, string, error) {
	// UUID v7: time-ordered, better B-tree index locality than v4.
	gameUUID, err := uuid.NewV7()
	if err != nil {
		return nil, "", fmt.Errorf("Manager.CreateGame userID=%s: generate game ID: %w", userID, err)
	}
	gameID := gameUUID.String()

	game := &store.Game{
		ID:            gameID,
		Status:        store.GameStatusWaiting,
		PlayerWhiteID: userID,
		CurrentFEN:    store.StartingFEN,
		WhiteTimeMs:   InitialTimeMs,
		BlackTimeMs:   InitialTimeMs,
	}
	if err := m.gameStore.CreateGame(ctx, game); err != nil {
		return nil, "", fmt.Errorf("Manager.CreateGame userID=%s: %w", userID, err)
	}

	token, err := m.signToken(gameID, userID, string(store.ColorWhite))
	if err != nil {
		return nil, "", fmt.Errorf("Manager.CreateGame gameID=%s: %w", gameID, err)
	}

	session := NewGameSession(gameID, userID)
	m.setClockTimeoutCallback(session)

	ch, unsubscribe, err := m.eventBus.Subscribe(ctx, gameID)
	if err != nil {
		return nil, "", fmt.Errorf("Manager.CreateGame gameID=%s: subscribe: %w", gameID, err)
	}
	m.startEventSubscriber(session, ch, unsubscribe)

	m.registry.Register(session)

	slog.Info("game created", "gameID", gameID, "userID", userID)
	return session, token, nil
}

// JoinGame lets userID join an existing game as Black. It validates the game
// is in WAITING_FOR_PLAYER status and that the user is not attempting self-play,
// updates the database, updates the in-memory session, and returns Black's
// signed player token.
//
// The caller is responsible for ensuring the user record exists before calling.
func (m *Manager) JoinGame(ctx context.Context, gameID, userID string) (string, error) {
	game, err := m.gameStore.GetGame(ctx, gameID)
	if err != nil {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, err)
	}

	if game.Status != store.GameStatusWaiting {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s: %w", gameID, ErrGameNotJoinable)
	}
	if game.PlayerBlackID != nil {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s: %w", gameID, ErrGameNotJoinable)
	}
	if game.PlayerWhiteID == userID {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, ErrSelfPlay)
	}

	if err := m.gameStore.UpdatePlayerBlack(ctx, gameID, userID); err != nil {
		if errors.Is(err, store.ErrGameNotJoinable) {
			// The conditional UPDATE's WHERE predicate failed: another request won
			// a concurrent join race for this exact gameID between our pre-flight
			// GetGame read above and this write. Translate to the game-package
			// sentinel — callers outside internal/store must never depend on a
			// store-package error type. See ADR-016.
			return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, ErrGameNotJoinable)
		}
		return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, err)
	}

	session, err := m.registry.Get(gameID)
	if err != nil {
		// Game is in DB but not in the registry. This indicates a server restart
		// between CreateGame and JoinGame — the game was WAITING and not restored
		// (RestoreActiveGames only loads ACTIVE games per GetActiveGames).
		slog.Error("Manager.JoinGame: game in DB but not in registry — server may have restarted",
			"gameID", gameID, "userID", userID)
		return "", fmt.Errorf("Manager.JoinGame gameID=%s: session not in registry: %w", gameID, err)
	}
	session.SetPlayerBlack(userID)

	token, err := m.signToken(gameID, userID, string(store.ColorBlack))
	if err != nil {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, err)
	}

	slog.Info("player joined game", "gameID", gameID, "userID", userID, "color", "BLACK")
	return token, nil
}

// HandleConnect registers a player's WebSocket connection into the correct
// GameSession slot and drives the connection lifecycle:
//
//   - First connect (WAITING): registers the connection; if both players are
//     now present, transitions to ACTIVE and starts the clock.
//   - Reconnect (ACTIVE, slot was cleared on prior disconnect): re-registers
//     the connection, cancels the abandonment timer, resumes the clock if both
//     players are now connected.
//   - Post-restart reconnect (ACTIVE, clock not yet started): same as reconnect
//     but calls clock.Start instead of clock.Resume when both players are present.
func (m *Manager) HandleConnect(ctx context.Context, gameID string, color store.Color, conn *ws.Connection) error {
	session, err := m.registry.Get(gameID)
	if err != nil {
		return fmt.Errorf("Manager.HandleConnect gameID=%s color=%s: %w", gameID, color, err)
	}

	// Snapshot status before registering so we can distinguish first-connect
	// (WAITING) from reconnect (ACTIVE).
	snap := session.CurrentStateSnapshot()

	regErr := session.RegisterConnection(color, conn)
	isOccupied := errors.Is(regErr, ErrConnectionOccupied)
	if isOccupied {
		// Slot is held by a stale pointer (simultaneous reconnect edge case).
		session.ReplaceConnection(color, conn)
	} else if regErr != nil {
		return fmt.Errorf("Manager.HandleConnect gameID=%s color=%s: register: %w",
			gameID, color, regErr)
	}

	// Cancel any pending abandonment timer for this player.
	m.cancelAbandonTimer(gameID, color)

	// Reconnect path: game is already ACTIVE (or we replaced a stale pointer).
	if isOccupied || snap.Status == store.GameStatusActive {
		m.sendGameState(session, color)
		m.sendSimple(session, opponentOf(color), MsgTypeOpponentReconnected)

		// Restart the clock only when both players are present — avoids ticking
		// down the clock while the opponent is still offline.
		if session.BothPlayersConnected() {
			currentSnap := session.CurrentStateSnapshot()
			if session.clock.IsStarted() {
				session.clock.Resume(currentSnap.Turn)
			} else {
				// Post-restart: clock was never started in this process.
				session.clock.Start(currentSnap.Turn)
			}
		}
		return nil
	}

	// First-connect path: game is WAITING_FOR_PLAYER.
	if !session.BothPlayersConnected() {
		m.sendGameState(session, color)
		return nil
	}

	// Both players now connected for the first time. Transition to ACTIVE.
	if err := session.Transition(store.GameStatusActive); err != nil {
		return fmt.Errorf("Manager.HandleConnect gameID=%s: transition to ACTIVE: %w", gameID, err)
	}

	// Persist status change. Non-fatal: in-memory state is authoritative.
	if err := m.gameStore.UpdateGameStatus(ctx, gameID, store.GameStatusActive, nil); err != nil {
		slog.Error("failed to persist ACTIVE status", "gameID", gameID, "error", err)
	}

	// White always moves first; start the clock for White.
	session.clock.Start(store.ColorWhite)

	// Send GAME_STATE to the connecting player; OPPONENT_CONNECTED to the waiting player.
	m.sendGameState(session, color)
	m.sendSimple(session, opponentOf(color), MsgTypeOpponentConnected)

	slog.Info("game is now ACTIVE", "gameID", gameID)
	return nil
}

// HandleDisconnect clears the player's connection slot, notifies the opponent,
// pauses the clock, and starts a 60-second abandonment timer. If the player
// reconnects before the timer fires, HandleConnect cancels it.
func (m *Manager) HandleDisconnect(gameID string, color store.Color) {
	session, err := m.registry.Get(gameID)
	if err != nil {
		return // Session already cleaned up (completed game).
	}

	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive && snap.Status != store.GameStatusWaiting {
		return // Terminal state — nothing to tear down.
	}

	session.ClearConnection(color)
	m.sendSimple(session, opponentOf(color), MsgTypeOpponentDisconnected)

	// Pause the active clock on any disconnect (TD-002).
	if session.clock.IsStarted() {
		session.clock.Pause()
	}

	m.startAbandonTimer(gameID, color)
	slog.Debug("player disconnected", "gameID", gameID, "color", color)
}

// HandleMessage parses and routes an incoming WebSocket message from a player.
// Routing:
//   - MOVE   → MoveProcessor.ProcessMove; MoveRejectionError → MOVE_REJECTED,
//     plain error → ERROR
//   - RESIGN → handleResign
//   - PING   → PONG to sender only
//   - unknown type → ERROR to sender
func (m *Manager) HandleMessage(ctx context.Context, gameID string, color store.Color, raw []byte) error {
	session, err := m.registry.Get(gameID)
	if err != nil {
		return fmt.Errorf("Manager.HandleMessage gameID=%s: %w", gameID, err)
	}

	var msg clientMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Warn("Manager.HandleMessage: invalid JSON from client",
			"gameID", gameID, "color", color, "error", err)
		m.sendError(session, color, ErrCodeInternalError, "invalid message format")
		return nil
	}

	switch msg.Type {
	case MsgTypeMove:
		gameEnded, moveErr := m.processor.ProcessMove(ctx, session, color, msg.SAN)
		if moveErr != nil {
			var rejection *MoveRejectionError
			if errors.As(moveErr, &rejection) {
				m.sendMoveRejected(session, color, msg.SAN, rejection.Reason)
			} else {
				slog.Error("Manager.HandleMessage: ProcessMove infrastructure failure",
					"gameID", gameID, "color", color, "san", msg.SAN, "error", moveErr)
				m.sendError(session, color, ErrCodeInternalError, "move processing failed")
			}
		}
		if gameEnded {
			// ProcessMove already transitioned the session to COMPLETED and
			// published GAME_OVER (handleGameOver) regardless of moveErr — the
			// DB persist failure path inside handleGameOver still leaves the
			// in-memory state terminal. finalizeGame must run exactly once here.
			m.finalizeGame(gameID)
		}

	case MsgTypeResign:
		m.handleResign(ctx, session, color)

	case MsgTypePing:
		m.sendSimple(session, color, MsgTypePong)

	default:
		slog.Warn("Manager.HandleMessage: unknown message type",
			"gameID", gameID, "color", color, "msgType", msg.Type)
		m.sendError(session, color, ErrCodeInternalError,
			fmt.Sprintf("unknown message type: %s", msg.Type))
	}

	return nil
}

// RestoreActiveGames is called once on server startup. It loads every game with
// an ACTIVE or WAITING_FOR_PLAYER status from the database, replays move history
// to reconstruct the board, and hydrates the GameRegistry so reconnecting players
// find their sessions.
//
// Sharp edges handled here (see CLAUDE.md Known Sharp Edges):
//   - Stale current_fen: board is always reconstructed via GameFromMoves, never
//     from games.current_fen.
//   - Zombie ACTIVE: if DetectOutcome finds the game is already over, the DB
//     record is corrected to COMPLETED and the session is not added to the registry.
//
// Individual game failures are logged and skipped — one bad record must not
// block the others.
func (m *Manager) RestoreActiveGames(ctx context.Context) error {
	games, err := m.gameStore.GetActiveGames(ctx)
	if err != nil {
		return fmt.Errorf("Manager.RestoreActiveGames: %w", err)
	}

	for _, game := range games {
		if err := m.restoreGame(ctx, game); err != nil {
			slog.Error("Manager.RestoreActiveGames: failed to restore game — skipping",
				"gameID", game.ID, "error", err)
		}
	}

	slog.Info("RestoreActiveGames complete", "count", len(games))
	return nil
}

// --- private methods ---------------------------------------------------------

func (m *Manager) restoreGame(ctx context.Context, game *store.Game) error {
	moves, err := m.moveStore.GetMovesForGame(ctx, game.ID)
	if err != nil {
		return fmt.Errorf("restoreGame gameID=%s: get moves: %w", game.ID, err)
	}

	sans := make([]string, len(moves))
	for i, mv := range moves {
		sans[i] = mv.SAN
	}

	board, err := internalchess.GameFromMoves(sans)
	if err != nil {
		return fmt.Errorf("restoreGame gameID=%s: replay moves: %w", game.ID, err)
	}

	// Zombie ACTIVE check: board is already in a terminal state but DB shows ACTIVE.
	// Happens when handleGameOver published GAME_OVER but UpdateGameStatus failed.
	if outcome, ended := m.validator.DetectOutcome(board); ended {
		slog.Warn("restoreGame: zombie ACTIVE game — correcting DB status",
			"gameID", game.ID, "outcome", outcome.Winner, "reason", outcome.Reason)
		storeOutcome := store.Outcome(outcome.Winner)
		storeReason := store.OutcomeReason(outcome.Reason)
		if dbErr := m.gameStore.UpdateGameStatus(ctx, game.ID, store.GameStatusCompleted, &store.GameOutcome{
			Outcome: storeOutcome,
			Reason:  storeReason,
		}); dbErr != nil {
			slog.Error("restoreGame: failed to correct zombie ACTIVE in DB",
				"gameID", game.ID, "error", dbErr)
		}
		return nil // Do not add to registry.
	}

	session := NewGameSessionFromDB(game, board)
	m.setClockTimeoutCallback(session)
	// Clock is not started here — it starts in HandleConnect when both players reconnect.

	ch, unsubscribe, err := m.eventBus.Subscribe(ctx, game.ID)
	if err != nil {
		return fmt.Errorf("restoreGame gameID=%s: subscribe: %w", game.ID, err)
	}
	m.startEventSubscriber(session, ch, unsubscribe)

	m.registry.Register(session)
	slog.Info("game restored", "gameID", game.ID, "status", game.Status, "moves", len(moves))
	return nil
}

func (m *Manager) handleResign(ctx context.Context, session *GameSession, color store.Color) {
	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive {
		return
	}

	session.clock.Stop()

	if err := session.Transition(store.GameStatusCompleted); err != nil {
		slog.Error("Manager.handleResign: transition failed",
			"gameID", session.ID, "color", color, "error", err)
		return
	}

	winner := opponentOf(color)
	winnerOutcome := store.Outcome(winner)
	storeReason := store.OutcomeReasonResignation
	session.SetOutcome(winnerOutcome, storeReason)

	if err := m.gameStore.UpdateGameStatus(ctx, session.ID, store.GameStatusCompleted, &store.GameOutcome{
		Outcome: winnerOutcome,
		Reason:  storeReason,
	}); err != nil {
		slog.Error("Manager.handleResign: failed to persist COMPLETED",
			"gameID", session.ID, "color", color, "error", err)
	}

	m.publishGameOver(ctx, session, string(winnerOutcome), string(storeReason),
		session.CurrentStateSnapshot().CurrentFEN)

	m.finalizeGame(session.ID)

	slog.Info("player resigned", "gameID", session.ID, "color", color, "winner", winner)
}

// handleTimeout is called by the Clock's background goroutine when a player's
// time reaches zero. Must not call any Clock method (deadlock risk — the clock
// goroutine holds no lock when calling this, but any re-entry into Clock would
// attempt to acquire clock.mu).
func (m *Manager) handleTimeout(gameID string, timedOut store.Color) {
	session, err := m.registry.Get(gameID)
	if err != nil {
		slog.Error("Manager.handleTimeout: session not found", "gameID", gameID)
		return
	}

	if err := session.Transition(store.GameStatusCompleted); err != nil {
		// TD-002 boundary race: game ended by checkmate or resign at the exact
		// same instant the clock fired. Expected no-op.
		slog.Debug("Manager.handleTimeout: transition failed — game already ended",
			"gameID", gameID, "timedOut", timedOut)
		return
	}

	winner := opponentOf(timedOut)
	winnerOutcome := store.Outcome(winner)
	storeReason := store.OutcomeReasonTimeout
	session.SetOutcome(winnerOutcome, storeReason)

	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusCompleted, &store.GameOutcome{
		Outcome: winnerOutcome,
		Reason:  storeReason,
	}); err != nil {
		slog.Error("Manager.handleTimeout: failed to persist COMPLETED",
			"gameID", gameID, "timedOut", timedOut, "error", err)
	}

	m.publishGameOver(context.Background(), session, string(winnerOutcome), string(storeReason),
		session.CurrentStateSnapshot().CurrentFEN)

	m.finalizeGame(gameID)

	slog.Info("player timed out", "gameID", gameID, "timedOut", timedOut, "winner", winner)
}

// onAbandonTimeout is called 60 seconds after a player's connection drops, if
// they have not reconnected by then (HandleConnect cancels this timer on
// reconnect). Per PHASE_1.md's state machine, single-player and both-players
// disconnection have different outcomes:
//
//   - If the opponent is still connected: the disconnected player loses by
//     abandonment. The game transitions to COMPLETED with the opponent as winner
//     and reason ABANDONED — not to ABANDONED status, since the game has a
//     definite winner, not a draw.
//   - If the opponent is also disconnected: the game transitions to ABANDONED
//     (terminal, no winner — recorded as a DRAW outcome with reason ABANDONED).
//
// color is the player whose 60-second timer fired — i.e. the player who has
// been disconnected the longest, not necessarily the only disconnected player.
func (m *Manager) onAbandonTimeout(gameID string, color store.Color) {
	key := abandonKey(gameID, color)
	m.mu.Lock()
	delete(m.abandonTimers, key)
	m.mu.Unlock()

	session, err := m.registry.Get(gameID)
	if err != nil {
		return
	}

	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive && snap.Status != store.GameStatusWaiting {
		return
	}

	session.clock.Stop()

	opponent := opponentOf(color)
	opponentConnected := session.IsPlayerConnected(opponent)

	if opponentConnected {
		// Single-player disconnect: opponent wins by abandonment. This is a
		// COMPLETED game with a winner, not an ABANDONED (drawn) one.
		if err := session.Transition(store.GameStatusCompleted); err != nil {
			slog.Debug("Manager.onAbandonTimeout: game already in terminal state",
				"gameID", gameID, "color", color)
			return
		}

		winnerOutcome := store.Outcome(opponent)
		session.SetOutcome(winnerOutcome, store.OutcomeReasonAbandoned)
		if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusCompleted, &store.GameOutcome{
			Outcome: winnerOutcome,
			Reason:  store.OutcomeReasonAbandoned,
		}); err != nil {
			slog.Error("Manager.onAbandonTimeout: failed to persist COMPLETED",
				"gameID", gameID, "error", err)
		}

		m.publishGameOver(context.Background(), session,
			string(winnerOutcome), string(store.OutcomeReasonAbandoned),
			session.CurrentStateSnapshot().CurrentFEN)

		m.finalizeGame(gameID)

		slog.Info("game ended by abandonment — opponent wins",
			"gameID", gameID, "disconnectedColor", color, "winner", opponent)
		return
	}

	// Both players disconnected: true abandonment, no winner.
	if err := session.Transition(store.GameStatusAbandoned); err != nil {
		slog.Debug("Manager.onAbandonTimeout: game already in terminal state",
			"gameID", gameID, "color", color)
		return
	}

	session.SetOutcome(store.OutcomeDraw, store.OutcomeReasonAbandoned)
	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusAbandoned, &store.GameOutcome{
		Outcome: store.OutcomeDraw,
		Reason:  store.OutcomeReasonAbandoned,
	}); err != nil {
		slog.Error("Manager.onAbandonTimeout: failed to persist ABANDONED",
			"gameID", gameID, "error", err)
	}

	m.publishGameOver(context.Background(), session,
		string(store.OutcomeDraw), string(store.OutcomeReasonAbandoned),
		session.CurrentStateSnapshot().CurrentFEN)

	m.finalizeGame(gameID)

	slog.Info("game abandoned — both players disconnected",
		"gameID", gameID, "disconnectedColor", color)
}

// finalizeGame performs bookkeeping common to every terminal path (MOVE-driven
// checkmate/stalemate via MoveProcessor.handleGameOver, handleResign, and
// onAbandonTimeout): it cancels any pending abandonment timers for both
// colors and removes the session from the GameRegistry.
//
// Must be called exactly once per game-ending event, after the session has
// already transitioned to a terminal state (COMPLETED or ABANDONED) and
// GAME_OVER has already been published. Calling it more than once is safe
// (cancelAbandonTimer and registry.Unregister are both no-ops on missing
// keys) but indicates a caller bug if it happens.
//
// Without this, completed/abandoned sessions remain in GameRegistry for the
// lifetime of the process — unbounded memory growth, not a goroutine leak
// (the EventBus subscriber already self-terminates on GAME_OVER).
func (m *Manager) finalizeGame(gameID string) {
	m.cancelAbandonTimer(gameID, store.ColorWhite)
	m.cancelAbandonTimer(gameID, store.ColorBlack)
	m.registry.Unregister(gameID)
}

func (m *Manager) publishGameOver(ctx context.Context, session *GameSession, outcome, reason, fen string) {
	payload, err := json.Marshal(gameOverMsg{
		Type:    MsgTypeGameOver,
		Outcome: outcome,
		Reason:  reason,
		FEN:     fen,
	})
	if err != nil {
		slog.Error("Manager.publishGameOver: marshal failed", "gameID", session.ID, "error", err)
		return
	}

	if err := m.eventBus.Publish(ctx, GameEvent{
		GameID:  session.ID,
		Type:    MsgTypeGameOver,
		Payload: payload,
	}); err != nil {
		slog.Error("Manager.publishGameOver: EventBus publish failed — sending directly",
			"gameID", session.ID, "error", err)
		session.SendToBothPlayers(payload)
	}
}

func (m *Manager) startEventSubscriber(session *GameSession, ch <-chan GameEvent, unsubscribe func()) {
	go func() {
		defer unsubscribe()
		for event := range ch {
			session.SendToBothPlayers(event.Payload)
			if event.Type == MsgTypeGameOver {
				return
			}
		}
	}()
}

func (m *Manager) setClockTimeoutCallback(session *GameSession) {
	gameID := session.ID
	session.clock.SetTimeoutCallback(func(timedOut store.Color) {
		m.handleTimeout(gameID, timedOut)
	})
}

func (m *Manager) startAbandonTimer(gameID string, color store.Color) {
	key := abandonKey(gameID, color)
	t := time.AfterFunc(abandonTimeout, func() {
		m.onAbandonTimeout(gameID, color)
	})
	m.mu.Lock()
	if old, ok := m.abandonTimers[key]; ok {
		old.Stop()
	}
	m.abandonTimers[key] = t
	m.mu.Unlock()
}

func (m *Manager) cancelAbandonTimer(gameID string, color store.Color) {
	key := abandonKey(gameID, color)
	m.mu.Lock()
	if t, ok := m.abandonTimers[key]; ok {
		t.Stop()
		delete(m.abandonTimers, key)
	}
	m.mu.Unlock()
}

func (m *Manager) sendGameState(session *GameSession, color store.Color) {
	snap := session.CurrentStateSnapshot()

	moves := snap.Moves
	if moves == nil {
		moves = []string{}
	}

	var outcomeStr, outcomeReasonStr *string
	if snap.Outcome != nil {
		s := string(*snap.Outcome)
		outcomeStr = &s
	}
	if snap.OutcomeReason != nil {
		s := string(*snap.OutcomeReason)
		outcomeReasonStr = &s
	}

	// Live clock read so reconnecting players see accurate remaining time.
	whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
	blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()

	payload, err := json.Marshal(gameStateMsg{
		Type:          MsgTypeGameState,
		FEN:           snap.CurrentFEN,
		Turn:          string(snap.Turn),
		Moves:         moves,
		Status:        string(snap.Status),
		WhiteTimeMs:   whiteMs,
		BlackTimeMs:   blackMs,
		Outcome:       outcomeStr,
		OutcomeReason: outcomeReasonStr,
	})
	if err != nil {
		slog.Error("Manager.sendGameState: marshal failed",
			"gameID", session.ID, "color", color, "error", err)
		return
	}

	if err := session.SendToPlayer(color, payload); err != nil {
		slog.Warn("Manager.sendGameState: send failed",
			"gameID", session.ID, "color", color, "error", err)
	}
}

func (m *Manager) sendSimple(session *GameSession, color store.Color, msgType string) {
	payload, _ := json.Marshal(simpleMsg{Type: msgType})
	if err := session.SendToPlayer(color, payload); err != nil {
		slog.Warn("Manager.sendSimple: send failed",
			"gameID", session.ID, "color", color, "msgType", msgType, "error", err)
	}
}

func (m *Manager) sendMoveRejected(session *GameSession, color store.Color, san, reason string) {
	payload, _ := json.Marshal(moveRejectedMsg{
		Type:   MsgTypeMoveRejected,
		SAN:    san,
		Reason: reason,
	})
	if err := session.SendToPlayer(color, payload); err != nil {
		slog.Warn("Manager.sendMoveRejected: send failed",
			"gameID", session.ID, "color", color, "error", err)
	}
}

func (m *Manager) sendError(session *GameSession, color store.Color, code, message string) {
	payload, _ := json.Marshal(errMsg{
		Type:    MsgTypeError,
		Code:    code,
		Message: message,
	})
	if err := session.SendToPlayer(color, payload); err != nil {
		slog.Warn("Manager.sendError: send failed",
			"gameID", session.ID, "color", color, "code", code, "error", err)
	}
}

func (m *Manager) signToken(gameID, userID, color string) (string, error) {
	claims := auth.PlayerClaims{
		GameID: gameID,
		UserID: userID,
		Color:  color,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	}
	token, err := auth.SignPlayerToken(claims, m.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("Manager.signToken gameID=%s userID=%s: %w", gameID, userID, err)
	}
	return token, nil
}

// opponentOf returns the opposing color.
func opponentOf(color store.Color) store.Color {
	if color == store.ColorWhite {
		return store.ColorBlack
	}
	return store.ColorWhite
}

// abandonKey returns the map key for a player's abandonment timer.
func abandonKey(gameID string, color store.Color) string {
	return gameID + ":" + string(color)
}
