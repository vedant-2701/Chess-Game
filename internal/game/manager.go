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

	// directory and instanceID are PHASE_2.md Step 5's additions — the
	// routing directory used by ResolveGame to determine game ownership, and
	// this process's own identity within it. Both are nil-safe zero values
	// (nil interface, empty string) for any caller that never calls
	// ResolveGame — e.g. existing tests exercising CreateGame/JoinGame/
	// RestoreActiveGames/HandleConnect are entirely unaffected by these
	// fields being unset.
	directory  RoutingDirectory
	instanceID string

	// mu protects: abandonTimers.
	mu            sync.Mutex
	abandonTimers map[string]*time.Timer // key: gameID+":"+string(color)
}

// NewManager constructs a Manager with all required dependencies.
//
// directory and instanceID are PHASE_2.md Step 5's additions, required for
// ResolveGame only. Pass directory=nil, instanceID="" for any caller that
// never calls ResolveGame (e.g. Phase 1 single-instance wiring, or tests
// exercising only CreateGame/JoinGame/HandleConnect/RestoreActiveGames) —
// ResolveGame is the only method that touches either field.
func NewManager(
	registry *GameRegistry,
	processor *MoveProcessor,
	gameStore *store.GameStore,
	moveStore *store.MoveStore,
	eventBus EventBus,
	jwtSecret string,
	validator *internalchess.Validator,
	directory RoutingDirectory,
	instanceID string,
) *Manager {
	return &Manager{
		registry:      registry,
		processor:     processor,
		gameStore:     gameStore,
		moveStore:     moveStore,
		eventBus:      eventBus,
		jwtSecret:     jwtSecret,
		validator:     validator,
		directory:     directory,
		instanceID:    instanceID,
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

	activated, regErr := session.RegisterConnection(color, conn)
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

	if activated {
		// Both players now connected for the first time. This goroutine atomically
		// transitioned the session to ACTIVE.

		// Persist status change. Non-fatal: in-memory state is authoritative.
		// fromStatus is always WAITING here — RegisterConnection only returns
		// activated=true via the WAITING→ACTIVE edge (session.go's
		// validTransitions has no other edge into ACTIVE).
		if err := m.gameStore.UpdateGameStatus(ctx, gameID, store.GameStatusWaiting, store.GameStatusActive, nil); err != nil {
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

	// First-connect path: game is WAITING_FOR_PLAYER and opponent is not yet connected.
	m.sendGameState(session, color)
	return nil
}

// HandleDisconnect clears the player's connection slot, notifies the opponent,
// pauses and persists the clock, and starts a 60-second abandonment timer. If
// the player reconnects before the timer fires, HandleConnect cancels it.
//
// ctx is the caller's (WSHandler's ADR-018 server-lifetime context, threaded
// through from ws.Connection.Start's onClose callback). HandleDisconnect now
// performs I/O (the clock persist below), so per CODING_GUIDELINES.md §2 it
// takes context.Context as its first argument — this was not required when
// the function was pure in-memory bookkeeping.
func (m *Manager) HandleDisconnect(ctx context.Context, gameID string, color store.Color) {
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

	// Pause the active clock on any disconnect (TD-002) and persist the
	// paused reading immediately.
	//
	// Prior to this fix, Pause() only updated in-memory state — the database
	// still held whatever was written after the game's last move. A player
	// who disconnects mid-turn, sits idle, and is then caught by a hard
	// kill -9 (no graceful shutdown, so no shutdown-time flush ever runs)
	// would resume on restart with extra time it shouldn't have: the elapsed
	// gap between the last move and the disconnect was never recorded.
	// Persisting here — at the moment of disconnect, not at shutdown — is
	// what actually closes that gap and is what PHASE_1.md acceptance
	// criterion #3 ("killing the server process ... resumes correctly")
	// depends on. See also PersistActiveClockState, which is a complementary,
	// not overlapping, fix for the *still-connected-at-graceful-shutdown*
	// case.
	if session.clock.IsStarted() {
		session.clock.Pause()

		whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
		blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()
		session.UpdateClocks(whiteMs, blackMs)

		// Deliberately NOT using ctx directly here. During graceful shutdown,
		// main.go's shutdown() cancels the ADR-018 server-lifetime context
		// BEFORE calling ws.Registry.CloseAll() (required so in-flight
		// HandleMessage calls observe cancellation before connections are
		// force-closed). CloseAll then triggers this exact code path for
		// every connected player via each connection's onClose callback — if
		// this call used ctx directly, it would fail with "context canceled"
		// on every single graceful shutdown, not as a rare edge case. Confirmed
		// by real E2E testing (PHASE_1.md Step 14): the first version of this
		// fix logged this exact error on every Ctrl+C.
		//
		// This write is a bounded, best-effort cleanup operation, not
		// something that should be aborted just because the broader
		// connection-lifetime context was cancelled — context.WithoutCancel
		// detaches it from that cancellation while still bounding it with its
		// own short timeout so a truly stuck DB call can't hang shutdown.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := m.gameStore.UpdateClocks(persistCtx, gameID, whiteMs, blackMs); err != nil {
			slog.Error("Manager.HandleDisconnect: failed to persist clock state on pause",
				"gameID", gameID, "color", color, "error", err)
		}
		cancel()
	}

	m.startAbandonTimer(gameID, color)
	slog.Debug("player disconnected", "gameID", gameID, "color", color)
}

// PersistActiveClockState flushes every currently-registered game's live
// clock reading to the database. Called once, during graceful shutdown
// (Step 13's "persist clock state" requirement), after ws.Registry.CloseAll
// has forced every connection closed.
//
// With HandleDisconnect's clock-persist fix in place, this is largely
// redundant for the common case: CloseAll disconnects every connected
// player, and each of those disconnects now self-persists via
// HandleDisconnect before CloseAll's wait returns. This method exists as
// defense-in-depth (a connection whose cleanup didn't complete before
// CloseAll's wait timeout would otherwise be missed) and, more importantly,
// as an explicit, auditable step in main.go's shutdown sequence that maps
// directly onto PHASE_1.md's checklist wording — the requirement shouldn't
// only be an emergent side effect of connection-cleanup ordering.
//
// This does not eliminate clock drift on an ungraceful kill -9: no signal is
// delivered in that case, so neither this method nor HandleDisconnect's own
// disconnect-triggered path ever runs for still-connected players. What
// HandleDisconnect's fix does guarantee is that drift is bounded by "time
// since the game's last move or last disconnect event," not by "time since
// the game's last move," regardless of how the process ends. That residual
// bound is an accepted Phase 1 limitation alongside TD-002, not something
// addressable without a periodic clock-persist ticker — out of Phase 1 scope.
func (m *Manager) PersistActiveClockState(ctx context.Context) {
	sessions := m.registry.AllActive()
	for _, session := range sessions {
		whiteMs := session.clock.TimeRemaining(store.ColorWhite).Milliseconds()
		blackMs := session.clock.TimeRemaining(store.ColorBlack).Milliseconds()
		if err := m.gameStore.UpdateClocks(ctx, session.ID, whiteMs, blackMs); err != nil {
			slog.Error("Manager.PersistActiveClockState: failed to persist clock state",
				"gameID", session.ID, "error", err)
		}
	}
	slog.Info("PersistActiveClockState complete", "count", len(sessions))
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

// GetGame returns the persisted game record for gameID. This is a thin
// passthrough to the store layer for read-only status queries (e.g. GET
// /games/:id, PHASE_1.md Step 12) that need no in-memory GameSession state.
// Routing it through Manager rather than giving internal/api a direct
// *store.GameStore dependency keeps internal/api's only dependency on the
// game layer as game.Manager, matching ARCHITECTURE.md's Dependency Graph.
func (m *Manager) GetGame(ctx context.Context, gameID string) (*store.Game, error) {
	g, err := m.gameStore.GetGame(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("Manager.GetGame gameID=%s: %w", gameID, err)
	}
	return g, nil
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
		// fromStatus is game.Status (not hardcoded ACTIVE): this correction runs
		// against a row freshly read via GetActiveGames, so game.Status is the
		// authoritative known-current value, whether WAITING or ACTIVE. In
		// practice a zombie terminal board can only occur for a game that was
		// ACTIVE (WAITING games have no moves), but using the actual field here
		// rather than assuming is the more defensive, self-documenting choice.
		if dbErr := m.gameStore.UpdateGameStatus(ctx, game.ID, game.Status, store.GameStatusCompleted, &store.GameOutcome{
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

	// fromStatus is always ACTIVE: session.Transition(COMPLETED) just
	// succeeded above, and validTransitions' only edge into COMPLETED is
	// from ACTIVE — a successful in-memory transition is proof of the prior
	// DB status, no separate snapshot needed.
	if err := m.gameStore.UpdateGameStatus(ctx, session.ID, store.GameStatusActive, store.GameStatusCompleted, &store.GameOutcome{
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

	// fromStatus is always ACTIVE — same reasoning as handleResign: a
	// successful Transition(COMPLETED) is only reachable from ACTIVE.
	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusActive, store.GameStatusCompleted, &store.GameOutcome{
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
		// fromStatus is always ACTIVE: Transition(COMPLETED) just succeeded, and
		// that edge only exists from ACTIVE. Note this means a WAITING game
		// whose sole creator disconnects and never returns can never reach this
		// branch's Transition call successfully in the first place — see the
		// separate, pre-existing gap flagged in this session's summary.
		if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusActive, store.GameStatusCompleted, &store.GameOutcome{
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
	// fromStatus is always ACTIVE: Transition(ABANDONED) just succeeded, and
	// that edge only exists from ACTIVE (same pre-existing WAITING-game gap
	// noted above applies here too).
	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusActive, store.GameStatusAbandoned, &store.GameOutcome{
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
//
// Deliberately does NOT close player WebSocket connections — do not add that
// here. finalizeGame runs on a different goroutine than whichever goroutine
// sent GAME_OVER (the EventBus subscriber, or publishGameOver's fallback),
// and closing connections from here would race the close frame against
// GAME_OVER's own delivery through the same per-connection outbound queue.
// See GameSession.CloseConnections' doc comment for the full reasoning and
// the two correct call sites.
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
		// Same-goroutine, immediately-after ordering as startEventSubscriber's
		// GAME_OVER branch — see CloseConnections' doc comment for why this
		// can't safely be done from finalizeGame instead.
		session.CloseConnections(wsCloseNormal, "game ended")
	}
}

func (m *Manager) startEventSubscriber(session *GameSession, ch <-chan GameEvent, unsubscribe func()) {
	go func() {
		defer unsubscribe()
		for event := range ch {
			session.SendToBothPlayers(event.Payload)
			if event.Type == MsgTypeGameOver {
				// GAME_OVER is terminal. Close both players' connections now,
				// in this same goroutine, immediately after the send above —
				// not from finalizeGame, which runs on a different goroutine
				// and would race this close frame against GAME_OVER's
				// delivery through the shared per-connection outbound queue
				// (LocalEventBus.Publish's channel send only guarantees the
				// event was enqueued for this goroutine to pick up, not that
				// this goroutine has run yet — see eventbus.go). Same-goroutine
				// program order is what actually guarantees GAME_OVER reaches
				// the wire before the close frame. See CloseConnections' doc
				// comment for the full reasoning.
				session.CloseConnections(wsCloseNormal, "game ended")
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
