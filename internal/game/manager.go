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

// firstMoveTimeout is DECISIONS_LOG_PHASE_3.md ADR-041's grace-period
// window: 20 seconds for White to play a first move after both players
// connect, re-armed once for Black's reply after White's first move
// persists. An order of magnitude tighter than abandonTimeout deliberately
// — this governs "has anyone actually started playing," not "has a
// mid-game player gone silent."
const firstMoveTimeout = 20 * time.Second

// matchedOpponentConnectTimeout is DECISIONS_LOG_PHASE_3.md ADR-042's
// timeout: how long a matched game waits, from the moment
// Manager.CreateMatchedGame creates it, for the assigned opponent to
// connect at all, before the game is aborted. Matched games only —
// shared-link games have no "assigned opponent," just an open slot anyone
// with the link can fill, and never arm this timer.
const matchedOpponentConnectTimeout = 60 * time.Second

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
	//
	// abandonTimers holds three distinct kinds of per-game timer, sharing one
	// map/mutex rather than three separate ones — they're never armed for
	// the same key at the same moment (mutually exclusive by construction,
	// see armTimersForGameStatus and HandleConnect's activated branch), so
	// there is no meaningful concurrency benefit to splitting them, only
	// three times the bookkeeping:
	//   - key = gameID+":"+string(color) (abandonKey): the ordinary per-color
	//     disconnect-abandon timer (60s, abandonTimeout).
	//   - key = gameID+":FIRSTMOVE" (firstMoveTimerKey): DECISIONS_LOG_PHASE_3.md
	//     ADR-041's first-move grace period (20s, firstMoveTimeout). Never
	//     collides with the per-color key shape — "FIRSTMOVE" is never a
	//     valid store.Color value.
	//   - key = gameID+":OPPONENT_NEVER_CONNECTED" (opponentNeverConnectedTimerKey):
	//     DECISIONS_LOG_PHASE_3.md ADR-042's matched-opponent-connect timeout
	//     (60s, matchedOpponentConnectTimeout). Same collision-safety
	//     reasoning.
	mu            sync.Mutex
	abandonTimers map[string]*time.Timer
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
	m := &Manager{
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
	// DECISIONS_LOG_PHASE_3.md ADR-041: wire the first-move grace period's
	// move-pipeline hook, mirroring setClockTimeoutCallback's existing
	// Clock→Manager pattern — processor must be non-nil (already a
	// pre-existing invariant: HandleMessage's MOVE case unconditionally calls
	// m.processor.ProcessMove for any test exercising it, nil or not).
	processor.setOnMovePersisted(m.onMovePersisted)
	return m
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

	// DECISIONS_LOG_PHASE_2.md ADR-028: claim Redis ownership for this game
	// immediately, on the same instance that just registered its live
	// GameSession locally. Without this, no instance holds an ownership
	// record for a freshly created game until some future resolve call
	// happens to claim it fresh — and the heartbeat ticker's batched renewal
	// (which walks registry.AllActive(), not the directory) tries to renew a
	// key that was never created, failing forever and logging "lost
	// ownership renewal" indefinitely for a game that was never actually lost.
	//
	// Best-effort / non-fatal: a Redis hiccup at creation time must not fail
	// game creation itself — the in-memory session and DB row are already
	// correct and authoritative regardless. A later resolve call's own
	// ClaimOwnership path handles "no owner recorded yet" identically whether
	// that's because this call never ran or because it failed here.
	if m.directory != nil {
		if _, claimErr := m.directory.ClaimOwnership(ctx, gameID, m.instanceID, ""); claimErr != nil {
			slog.Error("Manager.CreateGame: failed to claim initial ownership",
				"gameID", gameID, "instanceID", m.instanceID, "error", claimErr)
		}

		// DECISIONS_LOG_PHASE_3.md ADR-037: eagerly write White's
		// matchmaking_active_game marker now, at creation, rather than waiting
		// for the next heartbeat tick (Manager.renewActiveGameMarkers) —
		// closes what would otherwise be an ~OwnershipRenewInterval-wide gap
		// during which a just-created solo WAITING_FOR_PLAYER game's creator
		// could still slip a matchmaking-queue call through. Best-effort/
		// non-fatal, same reasoning as ClaimOwnership immediately above.
		if err := m.directory.SetActiveGameMarker(ctx, userID, ActiveGameMarker{
			GameID:        gameID,
			ConnectToken:  token,
			InstanceLabel: m.instanceID,
			WSPath:        "/connect/" + m.instanceID,
		}); err != nil {
			slog.Error("Manager.CreateGame: failed to set active-game marker",
				"gameID", gameID, "userID", userID, "error", err)
		}
	}

	slog.Info("game created", "gameID", gameID, "userID", userID)
	return session, token, nil
}

// JoinGame lets userID join an existing game as Black. It validates the game
// is in WAITING_FOR_PLAYER status and that the user is not attempting self-play,
// updates the database, and returns Black's signed player token.
//
// Deliberately a pure DB operation (DECISIONS_LOG_PHASE_2.md ADR-028): this
// method never touches GameRegistry or a live GameSession. Under round-robin
// REST routing, JoinGame can land on a different instance than the one
// CreateGame landed on — that instance's GameRegistry is a separate
// in-process map with no entry for a game it never created or hydrated, so
// reaching into it here would be asking the wrong process's memory for
// something Postgres already answers correctly. Nothing in the WebSocket
// message protocol reads GameSession.playerBlackID — connect-flow identity
// comes from signed JWT claims — and any GameSession later legitimately
// hydrated from the DB (hydrateGameSession/NewGameSessionFromDB) picks up
// player_black_id from the row this method writes, automatically, with no
// extra step needed here.
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

	token, err := m.signToken(gameID, userID, string(store.ColorBlack))
	if err != nil {
		return "", fmt.Errorf("Manager.JoinGame gameID=%s userID=%s: %w", gameID, userID, err)
	}

	if m.directory != nil {
		// DECISIONS_LOG_PHASE_3.md ADR-037: eagerly write Black's marker at
		// join time. instanceLabel is looked up via GetOwner rather than just
		// using m.instanceID: JoinGame deliberately never claims or even knows
		// ownership itself (see this method's own doc comment above) — under
		// round-robin REST routing, THIS call can land on a different instance
		// than the one actually hosting the live GameSession. Using the wrong
		// instanceLabel here would send a matchmaking-service 409 response's
		// connect info somewhere that just fails the WS handshake — harmless
		// (the client falls back to /resolve, same as any other stale-label
		// case), but avoidable with one cheap lookup. Falls back to this
		// instance's own ID only if no ownership record exists at all yet
		// (nobody has connected to this game on any instance) — a reasonable
		// best guess in that edge case, not a correctness requirement.
		instanceLabel := m.instanceID
		if owner, ok, ownerErr := m.directory.GetOwner(ctx, gameID); ownerErr == nil && ok {
			instanceLabel = owner
		}
		if err := m.directory.SetActiveGameMarker(ctx, userID, ActiveGameMarker{
			GameID:        gameID,
			ConnectToken:  token,
			InstanceLabel: instanceLabel,
			WSPath:        "/connect/" + instanceLabel,
		}); err != nil {
			slog.Error("Manager.JoinGame: failed to set active-game marker",
				"gameID", gameID, "userID", userID, "error", err)
		}
	}

	slog.Info("player joined game", "gameID", gameID, "userID", userID, "color", "BLACK")
	return token, nil
}

// CreateMatchedGame atomically creates a new game for two players already
// paired by chess-server's pairing loop (internal/matchmaking, Phase 3 Step
// 3) — DECISIONS_LOG_PHASE_3.md ADR-032/ADR-034. Unlike CreateGame (single
// player, WAITING_FOR_PLAYER, joined later via JoinGame), both players are
// already known: this is Manager's counterpart to GameStore.CreateMatchedGame,
// adding the same eager local-registration/ownership-claim pattern CreateGame
// already establishes, PLUS arming DECISIONS_LOG_PHASE_3.md ADR-042's
// matchedOpponentConnectTimeout in the same call — closing TD-P3-004 requires
// that timer to be armed at the exact moment the game becomes visible to
// either player, not as an afterthought.
//
// matchmakingRequestID must be generated once by the caller (the pairing
// loop: one UUID v4 per ZPOPMIN-won pairing attempt) and reused verbatim
// across every retry of that same attempt — this is what makes retrying
// after an ambiguous failure safe rather than a double-booking risk. See
// GameStore.CreateMatchedGame's doc comment for the full mechanism.
//
// Idempotent-retry path: if a prior call with this exact matchmakingRequestID
// already committed (GameStore.CreateMatchedGame reports inserted==false),
// this method looks the existing game up by requestID and returns its
// session rather than erroring or creating a duplicate.
//
// Known, accepted narrow gap in that retry path (new, flagged explicitly
// rather than left silent): it re-hydrates/re-registers the session and
// reissues tokens, but does NOT re-claim ownership or re-arm the
// opponent-connect timer — both are assumed to have already run to
// completion as part of the original call that produced inserted==true,
// since neither involves any I/O between the atomic insert and the point
// where a caller-observable failure could plausibly interrupt this method.
// A failure narrow enough to succeed the insert but fail before
// ClaimOwnership/arming would leave that specific game under-protected until
// its next connect/resolve event — same shape and severity as the already-
// accepted residual gaps this project tracks elsewhere (TD-P2-006, ADR-042's
// own accepted residual for the crash+nobody-ever-resolves case).
func (m *Manager) CreateMatchedGame(ctx context.Context, playerWhiteID, playerBlackID, matchmakingRequestID string) (session *GameSession, whiteToken, blackToken string, err error) {
	gameUUID, err := uuid.NewV7()
	if err != nil {
		return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame whiteID=%s blackID=%s: generate game ID: %w",
			playerWhiteID, playerBlackID, err)
	}
	gameID := gameUUID.String()

	reqID := matchmakingRequestID
	blackID := playerBlackID
	game := &store.Game{
		ID:                   gameID,
		Status:               store.GameStatusWaiting,
		PlayerWhiteID:        playerWhiteID,
		PlayerBlackID:        &blackID,
		CurrentFEN:           store.StartingFEN,
		WhiteTimeMs:          InitialTimeMs,
		BlackTimeMs:          InitialTimeMs,
		MatchmakingRequestID: &reqID,
	}

	inserted, err := m.gameStore.CreateMatchedGame(ctx, game)
	if err != nil {
		return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame whiteID=%s blackID=%s requestID=%s: %w",
			playerWhiteID, playerBlackID, matchmakingRequestID, err)
	}

	if !inserted {
		// Idempotent retry — see doc comment above for the accepted gap here.
		existing, getErr := m.gameStore.GetGameByMatchmakingRequestID(ctx, matchmakingRequestID)
		if getErr != nil {
			return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame requestID=%s: retry lookup: %w",
				matchmakingRequestID, getErr)
		}
		if existing.PlayerBlackID == nil {
			// Unreachable in practice: this method is the only writer of a row
			// with this MatchmakingRequestID, and it always sets PlayerBlackID.
			// Defensive, not a real expected path.
			return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame requestID=%s: existing game %s has no PlayerBlackID",
				matchmakingRequestID, existing.ID)
		}

		hydrated, hydrateErr := m.registry.GetOrHydrate(ctx, existing.ID, func(hydrateCtx context.Context) (*GameSession, error) {
			return m.hydrateGameSession(hydrateCtx, existing.ID)
		})
		if hydrateErr != nil {
			return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame requestID=%s: hydrate existing game %s: %w",
				matchmakingRequestID, existing.ID, hydrateErr)
		}

		whiteToken, err = m.signToken(existing.ID, existing.PlayerWhiteID, string(store.ColorWhite))
		if err != nil {
			return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame requestID=%s: %w", matchmakingRequestID, err)
		}
		blackToken, err = m.signToken(existing.ID, *existing.PlayerBlackID, string(store.ColorBlack))
		if err != nil {
			return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame requestID=%s: %w", matchmakingRequestID, err)
		}

		slog.Info("matched game idempotent retry resolved to existing game",
			"gameID", existing.ID, "requestID", matchmakingRequestID)

		if m.directory != nil {
			// DECISIONS_LOG_PHASE_3.md ADR-037: re-set (not just accept the
			// original attempt's write) both markers here too — unlike
			// ownership/the opponent-connect timer (this method's own doc
			// comment's accepted gap), a stale-but-still-correct-content marker
			// costs nothing to refresh, and this marker's PRIMARY purpose
			// (preventing double-queueing, not just connect-routing convenience)
			// is worth the small extra correctness margin. m.instanceID is a
			// reasonable label here (unlike JoinGame's genuinely ambiguous
			// cross-instance case): GetOrHydrate above just succeeded on THIS
			// instance, which normally means this instance now legitimately
			// owns the session.
			wsPath := "/connect/" + m.instanceID
			if err := m.directory.SetActiveGameMarker(ctx, existing.PlayerWhiteID, ActiveGameMarker{
				GameID: existing.ID, ConnectToken: whiteToken, InstanceLabel: m.instanceID, WSPath: wsPath,
			}); err != nil {
				slog.Error("Manager.CreateMatchedGame: failed to set white active-game marker (retry path)",
					"gameID", existing.ID, "userID", existing.PlayerWhiteID, "error", err)
			}
			if err := m.directory.SetActiveGameMarker(ctx, *existing.PlayerBlackID, ActiveGameMarker{
				GameID: existing.ID, ConnectToken: blackToken, InstanceLabel: m.instanceID, WSPath: wsPath,
			}); err != nil {
				slog.Error("Manager.CreateMatchedGame: failed to set black active-game marker (retry path)",
					"gameID", existing.ID, "userID", *existing.PlayerBlackID, "error", err)
			}
		}

		return hydrated, whiteToken, blackToken, nil
	}

	whiteToken, err = m.signToken(gameID, playerWhiteID, string(store.ColorWhite))
	if err != nil {
		return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame gameID=%s: %w", gameID, err)
	}
	blackToken, err = m.signToken(gameID, playerBlackID, string(store.ColorBlack))
	if err != nil {
		return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame gameID=%s: %w", gameID, err)
	}

	// NewGameSessionFromDB (not NewGameSession+SetPlayerBlack): game already
	// carries both player IDs, and this constructor sets both from a single
	// *store.Game in one shot — internalchess.NewGame() supplies the fresh
	// starting-position board a brand-new game needs, matching NewGameSession's
	// own board initialization for the shared-link path.
	session = NewGameSessionFromDB(game, internalchess.NewGame())
	m.setClockTimeoutCallback(session)

	ch, unsubscribe, err := m.eventBus.Subscribe(ctx, gameID)
	if err != nil {
		return nil, "", "", fmt.Errorf("Manager.CreateMatchedGame gameID=%s: subscribe: %w", gameID, err)
	}
	m.startEventSubscriber(session, ch, unsubscribe)

	m.registry.Register(session)

	// DECISIONS_LOG_PHASE_2.md ADR-028's same reasoning as CreateGame: claim
	// ownership immediately, on the same instance that just registered the
	// live session locally. Best-effort/non-fatal — a Redis hiccup here must
	// not fail game creation; the in-memory session and DB row are already
	// correct and authoritative regardless.
	if m.directory != nil {
		if _, claimErr := m.directory.ClaimOwnership(ctx, gameID, m.instanceID, ""); claimErr != nil {
			slog.Error("Manager.CreateMatchedGame: failed to claim initial ownership",
				"gameID", gameID, "instanceID", m.instanceID, "error", claimErr)
		}

		// DECISIONS_LOG_PHASE_3.md ADR-037: eagerly write both players'
		// markers now — same reasoning as CreateGame's own eager write. Matched
		// games have both players known upfront, so both markers land in this
		// one call, unlike the shared-link flow's necessarily two-step
		// CreateGame+JoinGame.
		wsPath := "/connect/" + m.instanceID
		if err := m.directory.SetActiveGameMarker(ctx, playerWhiteID, ActiveGameMarker{
			GameID: gameID, ConnectToken: whiteToken, InstanceLabel: m.instanceID, WSPath: wsPath,
		}); err != nil {
			slog.Error("Manager.CreateMatchedGame: failed to set white active-game marker",
				"gameID", gameID, "userID", playerWhiteID, "error", err)
		}
		if err := m.directory.SetActiveGameMarker(ctx, playerBlackID, ActiveGameMarker{
			GameID: gameID, ConnectToken: blackToken, InstanceLabel: m.instanceID, WSPath: wsPath,
		}); err != nil {
			slog.Error("Manager.CreateMatchedGame: failed to set black active-game marker",
				"gameID", gameID, "userID", playerBlackID, "error", err)
		}
	}

	// DECISIONS_LOG_PHASE_3.md ADR-042 — closes TD-P3-004: arm the
	// matched-opponent-connect timeout now, in the same call that created and
	// registered this game, not as a separate follow-up step a caller could
	// forget. Cancelled by HandleConnect's activated branch the moment both
	// players are actually present.
	m.armOpponentNeverConnectedTimer(gameID)

	slog.Info("matched game created", "gameID", gameID, "whiteID", playerWhiteID, "blackID", playerBlackID,
		"requestID", matchmakingRequestID)
	return session, whiteToken, blackToken, nil
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
		// PHASE_2.md Step 8: fall back to hydrate-on-miss, regardless of why
		// the local registry missed — never previously owned by this instance,
		// a post-failover connect, or a fast-restart-with-empty-registry
		// (DECISIONS_LOG_PHASE_2.md ADR-024) all look identical to this code
		// and are all handled correctly by the same path. This is now the
		// single mechanism responsible for "does this instance have this
		// game's session in memory" — see ADR-024's Consequences, and note
		// cmd/server/main.go no longer calls RestoreActiveGames eagerly at
		// startup as of this same change, per that ADR.
		hydrated, hydrateErr := m.registry.GetOrHydrate(ctx, gameID, func(hydrateCtx context.Context) (*GameSession, error) {
			return m.hydrateGameSession(hydrateCtx, gameID)
		})
		if hydrateErr != nil {
			return fmt.Errorf("Manager.HandleConnect gameID=%s color=%s: %w", gameID, color, hydrateErr)
		}
		session = hydrated
		// DECISIONS_LOG_PHASE_2.md ADR-030: resume any pending abandonment
		// grace period this freshly-hydrated session has no in-memory record
		// of. Safe to call after GetOrHydrate returns — registration into
		// GameRegistry is guaranteed complete by then. DECISIONS_LOG_PHASE_3.md
		// ADR-041: armTimersForGame (not the narrower, now-removed
		// armAbandonTimersForGame) also arms the first-move timer instead, for
		// an ACTIVE game still inside that window — see armTimersForGameStatus.
		m.armTimersForGame(ctx, gameID)
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
	// DECISIONS_LOG_PHASE_2.md ADR-030: clear the persisted disconnect
	// marker so a later failover doesn't see a stale timestamp and think this
	// player is still disconnected. Best-effort/non-fatal, matching the
	// existing precedent for Redis ownership claims in CreateGame: a DB
	// hiccup here must not fail an otherwise-successful connect, and worst
	// case a lingering timestamp only matters if this exact game later fails
	// over again while still ACTIVE/WAITING with this color's slot empty
	// once more — a narrow, bounded residual risk, not a silent
	// mis-resolution of the current connect.
	if err := m.gameStore.UpdateDisconnectTimestamp(ctx, gameID, color, nil); err != nil {
		slog.Error("Manager.HandleConnect: failed to clear disconnect timestamp",
			"gameID", gameID, "color", color, "error", err)
	}

	if activated {
		// Both players now connected for the first time. This goroutine atomically
		// transitioned the session to ACTIVE.

		// Persist status change AND the ADR-041 activation timestamp in one
		// statement. Non-fatal: in-memory state is authoritative. Replaces the
		// prior plain UpdateGameStatus(...ACTIVE...) call — ActivateGame is
		// the only writer of games.activated_at, ever.
		if err := m.gameStore.ActivateGame(ctx, gameID); err != nil {
			slog.Error("failed to persist ACTIVE status", "gameID", gameID, "error", err)
		}

		// DECISIONS_LOG_PHASE_3.md ADR-042: the opponent showed up — the
		// "we found you a match" promise this timer exists to enforce is kept,
		// the concern is moot from here forward regardless of what happens
		// next. Safe no-op for a shared-link game, which never arms this timer
		// in the first place (Manager.CreateMatchedGame, Phase 3 Step 3, is
		// the only arm site) — cancelTimer is a no-op on a missing key, same
		// as cancelAbandonTimer already relies on elsewhere in this file.
		m.cancelOpponentNeverConnectedTimer(gameID)

		// DECISIONS_LOG_PHASE_3.md ADR-041: arm White's first-move grace-period
		// window. This is the sole abandonment-governing mechanism for this
		// game until Black's second-move-count threshold is reached (see the
		// move-pipeline integration in move.go) — there is nothing to
		// separately suppress here: this live activation path has never armed
		// a per-color abandon timer to begin with (only HandleDisconnect does
		// that), so "don't arm the ordinary timer during this window" is
		// already true by construction on this path. The suppression that
		// actually needs explicit gating is at hydration — see
		// armTimersForGameStatus.
		m.armFirstMoveTimer(gameID, firstMoveTimeout)

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

	// DECISIONS_LOG_PHASE_2.md ADR-030: persist the disconnect moment so a
	// surviving instance can resume (or immediately resolve) this grace
	// period after a failover — the in-memory timer just armed above is pure
	// per-process memory and does not survive this process dying. Detached
	// from ctx's cancellation for the same reason as the clock-persist write
	// above (ADR-019): during graceful shutdown this fires from every
	// still-connected player's disconnect, after ctx is already cancelled.
	// Best-effort — the LOCAL timer already governs this process's own
	// behavior regardless of whether this write succeeds; only
	// failover-survival degrades if it doesn't.
	disconnectedAt := time.Now()
	persistDisconnectCtx, cancelDisconnect := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if err := m.gameStore.UpdateDisconnectTimestamp(persistDisconnectCtx, gameID, color, &disconnectedAt); err != nil {
		slog.Error("Manager.HandleDisconnect: failed to persist disconnect timestamp",
			"gameID", gameID, "color", color, "error", err)
	}
	cancelDisconnect()

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
	// DECISIONS_LOG_PHASE_2.md ADR-030/ADR-031: resume any pending abandonment
	// grace period from persisted DB timestamps, falling back to "assume
	// disconnected as of now" when no timestamp was ever written but the game
	// is non-terminal (effectiveDisconnectedAt) — Manager.abandonTimers is
	// pure per-process memory and does not survive the process being
	// restarted, which is exactly the scenario restoreGame exists for.
	// DECISIONS_LOG_PHASE_3.md ADR-041: game and moves are already both in
	// scope here (moves was fetched above for board reconstruction), so this
	// calls armTimersForGameStatus directly rather than the data-fetching
	// armTimersForGame wrapper used by HandleConnect/ResolveGame, which don't
	// already have both values loaded.
	m.armTimersForGameStatus(game, moves)
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

	// DECISIONS_LOG_PHASE_2.md ADR-029: a game that never left
	// WAITING_FOR_PLAYER has no opponent to have "abandoned" and no winner to
	// declare — it never started. This must be handled BEFORE the
	// single/both-disconnected branching below, which was designed for a game
	// where both players had actually joined (ACTIVE) and is the wrong
	// outcome shape here: it would either award a phantom "opponent wins"
	// against an opponent who never existed, or score a DRAW for a game with
	// no real stakes. Real chess platforms treat this as void (aborted), not
	// a scored draw — same principle here.
	if snap.Status == store.GameStatusWaiting {
		if err := session.Transition(store.GameStatusAborted); err != nil {
			slog.Debug("Manager.onAbandonTimeout: game already in terminal state",
				"gameID", gameID, "color", color)
			return
		}

		// fromStatus is always WAITING: Transition(ABORTED) just succeeded, and
		// that edge only exists from WAITING (session.go's validTransitions).
		// outcome is nil — an aborted game has no result to record, unlike a
		// COMPLETED or ABANDONED one.
		if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusWaiting, store.GameStatusAborted, nil); err != nil {
			slog.Error("Manager.onAbandonTimeout: failed to persist ABORTED",
				"gameID", gameID, "error", err)
		}

		// Outcome/reason are deliberately empty strings, not store.Outcome/
		// store.OutcomeReason constants — ABORTED has no entry in either DB
		// CHECK constraint (outcome/outcome_reason stay NULL above), since it
		// isn't a scored result in that taxonomy. "ABORTED" here is a
		// wire-only signal for the GAME_OVER payload, not a persisted value.
		m.publishGameOver(context.Background(), session, "", "ABORTED",
			session.CurrentStateSnapshot().CurrentFEN)

		m.finalizeGame(gameID)

		slog.Info("game aborted — opponent never joined",
			"gameID", gameID, "creatorColor", color)
		return
	}

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

// onFirstMoveTimeout is called when DECISIONS_LOG_PHASE_3.md ADR-041's
// first-move grace period elapses — 20 seconds after the game became ACTIVE
// (White's window), or 20 seconds after White's first move persisted
// (Black's window, re-armed by the move pipeline — see move.go).
//
// Gated at fire-time on the actual current move count, not just on "this
// timer wasn't cancelled": a stale fire (the callback was already scheduled
// before a just-landed move retired or re-armed the mechanism — the classic
// timer-vs-mutation race any time.AfterFunc-based mechanism has) must not
// abort a game that has, in fact, already progressed past the threshold this
// specific timer instance was guarding. len(snap.Moves) >= 2 means the
// mechanism has already retired (Black's reply landed); a first-move timer
// firing after that point is always stale and must no-op.
func (m *Manager) onFirstMoveTimeout(gameID string) {
	m.cancelTimer(firstMoveTimerKey(gameID))

	session, err := m.registry.Get(gameID)
	if err != nil {
		return
	}

	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusActive {
		return // Already terminal — stale fire, no-op.
	}
	if len(snap.Moves) >= 2 {
		return // Mechanism already retired (Black replied) — stale fire, no-op.
	}

	if err := session.Transition(store.GameStatusAborted); err != nil {
		slog.Debug("Manager.onFirstMoveTimeout: game already in terminal state", "gameID", gameID)
		return
	}

	session.clock.Stop()

	// fromStatus is always ACTIVE: Transition(ABORTED) just succeeded, and
	// that edge only exists from ACTIVE (session.go's validTransitions,
	// ADR-041). outcome is nil — an aborted game has no result to record,
	// same reasoning as onAbandonTimeout's pre-existing WAITING→ABORTED
	// branch (ADR-029).
	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusActive, store.GameStatusAborted, nil); err != nil {
		slog.Error("Manager.onFirstMoveTimeout: failed to persist ABORTED",
			"gameID", gameID, "error", err)
	}

	// Outcome/reason are the same wire-only "ABORTED" signal used by
	// onAbandonTimeout's WAITING branch — not a store.Outcome/
	// store.OutcomeReason constant, since ABORTED has no entry in either DB
	// CHECK constraint.
	m.publishGameOver(context.Background(), session, "", "ABORTED",
		session.CurrentStateSnapshot().CurrentFEN)

	m.finalizeGame(gameID)

	slog.Info("game aborted — first-move grace period elapsed",
		"gameID", gameID, "movesPlayed", len(snap.Moves))
}

// onOpponentNeverConnectedTimeout is called 60 seconds after
// Manager.CreateMatchedGame creates a matched game, if the assigned
// opponent never connected at all (DECISIONS_LOG_PHASE_3.md ADR-042 —
// closes TD-P3-004). Cancelled by HandleConnect's activated branch the
// moment both players are actually present; this callback only ever fires
// when that never happened.
//
// Gated on game.Status == WAITING_FOR_PLAYER at fire-time for the same
// stale-fire reasoning as onFirstMoveTimeout — Transition's own idempotency
// (fails harmlessly on an already-ACTIVE or already-terminal session) is the
// actual safety net every timeout handler in this file relies on; this
// status check is a fast-path short-circuit, not the only thing preventing
// a double-fire.
//
// Does not touch the ADR-037 matchmaking_active_game Redis marker: ADR-042's
// own Consequences record that the absent player's marker keeps renewing
// for up to matchedOpponentConnectTimeout after this fires, blocking their
// own re-queue for that same window — a deliberately accepted, bounded
// consequence (the marker's own TTL-based expiry resolves it independently
// once ADR-037's heartbeat.go extension exists), not a gap this function
// needs to close.
func (m *Manager) onOpponentNeverConnectedTimeout(gameID string) {
	m.cancelTimer(opponentNeverConnectedTimerKey(gameID))

	session, err := m.registry.Get(gameID)
	if err != nil {
		return
	}

	snap := session.CurrentStateSnapshot()
	if snap.Status != store.GameStatusWaiting {
		return // Opponent connected (now ACTIVE), or the game already ended some other way — no-op.
	}

	if err := session.Transition(store.GameStatusAborted); err != nil {
		slog.Debug("Manager.onOpponentNeverConnectedTimeout: game already in terminal state", "gameID", gameID)
		return
	}

	// fromStatus is always WAITING, same reasoning as onAbandonTimeout's
	// existing WAITING→ABORTED branch (ADR-029). outcome is nil, same
	// reason: an aborted game has no scored result.
	if err := m.gameStore.UpdateGameStatus(context.Background(), gameID, store.GameStatusWaiting, store.GameStatusAborted, nil); err != nil {
		slog.Error("Manager.onOpponentNeverConnectedTimeout: failed to persist ABORTED",
			"gameID", gameID, "error", err)
	}

	m.publishGameOver(context.Background(), session, "", "ABORTED",
		session.CurrentStateSnapshot().CurrentFEN)

	m.finalizeGame(gameID)

	slog.Info("matched game aborted — assigned opponent never connected", "gameID", gameID)
}

// onMovePersisted implements DECISIONS_LOG_PHASE_3.md ADR-041's move-count-
// gated first-move grace period, wired into MoveProcessor via
// MoveProcessor.setOnMovePersisted (called once, from NewManager) — mirrors
// setClockTimeoutCallback's existing Clock→Manager pattern.
//
//   - moveNumber == 1 (White's move just persisted): arm Black's window,
//     the full firstMoveTimeout. This fires at essentially the exact moment
//     of persistence, so time.Now() introduces no meaningful drift here,
//     unlike the hydration-time case (armTimersForGameStatus), which must
//     recompute precisely from moves[0].PlayedAt because it can run
//     arbitrarily long after the fact.
//   - moveNumber == 2 (Black's reply just persisted): the mechanism has done
//     its job and retires. Cancels the first-move timer — best-effort; it
//     may already be racing to fire on its own goroutine, but
//     onFirstMoveTimeout's own move-count re-check at fire-time makes that
//     race safe regardless of which side wins — and bootstraps the ordinary
//     per-color abandon timer for any color disconnected AT THIS EXACT
//     MOMENT (sub-decision 3's gap: without this, a player who disconnected
//     during the first-move window, when the ordinary timer was
//     deliberately not armed, would have no abandon timer running at all
//     once the window retires, until their next connect/disconnect event).
//     Uses the real persisted disconnect timestamp
//     (armAbandonTimerForColor/effectiveDisconnectedAt, same machinery
//     hydration already uses) rather than a needlessly generous fresh 60s
//     window — a player silent since well before this exact moment
//     shouldn't get extra grace just because the bootstrap happens to run
//     now. Only reads the game row when at least one color is actually
//     disconnected — the common case (both players still present through
//     the opening moves) costs no extra I/O at all.
//   - Any other moveNumber: no-op.
func (m *Manager) onMovePersisted(ctx context.Context, gameID string, moveNumber int) {
	switch moveNumber {
	case 1:
		m.armFirstMoveTimer(gameID, firstMoveTimeout)

	case 2:
		m.cancelFirstMoveTimer(gameID)

		session, err := m.registry.Get(gameID)
		if err != nil {
			return
		}

		whiteConnected := session.IsPlayerConnected(store.ColorWhite)
		blackConnected := session.IsPlayerConnected(store.ColorBlack)
		if whiteConnected && blackConnected {
			return
		}

		game, err := m.gameStore.GetGame(ctx, gameID)
		if err != nil {
			slog.Error("Manager.onMovePersisted: failed to read game for disconnected-player bootstrap",
				"gameID", gameID, "error", err)
			return
		}
		if !whiteConnected {
			m.armAbandonTimerForColor(gameID, store.ColorWhite, effectiveDisconnectedAt(store.GameStatusActive, game.WhiteDisconnectedAt))
		}
		if !blackConnected {
			m.armAbandonTimerForColor(gameID, store.ColorBlack, effectiveDisconnectedAt(store.GameStatusActive, game.BlackDisconnectedAt))
		}
	}
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
	// DECISIONS_LOG_PHASE_3.md ADR-041/ADR-042: both new timer kinds are
	// per-game, not per-color, and both must be cancelled on every terminal
	// path exactly like the per-color timers above — cancelTimer is a no-op
	// on a missing key, so this is safe even for the (overwhelmingly common)
	// case where neither was ever armed for this particular game.
	m.cancelFirstMoveTimer(gameID)
	m.cancelOpponentNeverConnectedTimer(gameID)
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
	m.startAbandonTimerWithDuration(gameID, color, abandonTimeout)
}

// armTimer arms (or re-arms, replacing any existing timer at this key) a
// timer under Manager.abandonTimers, calling onFire when it elapses. Shared
// plumbing for the ordinary per-color abandon timer
// (startAbandonTimerWithDuration) and the DECISIONS_LOG_PHASE_3.md
// ADR-041/ADR-042 timers (armFirstMoveTimer, armOpponentNeverConnectedTimer)
// — all three share the same map/mutex, keyed by collision-safe suffixes
// (see abandonTimers' doc comment on the Manager struct).
func (m *Manager) armTimer(key string, d time.Duration, onFire func()) {
	t := time.AfterFunc(d, onFire)
	m.mu.Lock()
	if old, ok := m.abandonTimers[key]; ok {
		old.Stop()
	}
	m.abandonTimers[key] = t
	m.mu.Unlock()
}

// cancelTimer stops and removes the timer at key, if any is armed. Safe
// no-op if absent — several callers rely on exactly this (e.g. HandleConnect
// cancelling ADR-042's opponent-connect timer unconditionally, even for a
// shared-link game that never armed it in the first place).
func (m *Manager) cancelTimer(key string) {
	m.mu.Lock()
	if t, ok := m.abandonTimers[key]; ok {
		t.Stop()
		delete(m.abandonTimers, key)
	}
	m.mu.Unlock()
}

// startAbandonTimerWithDuration arms (or re-arms, replacing any existing
// timer for this key) an abandonment timer with an explicit duration rather
// than always the full abandonTimeout. Used by armAbandonTimerForColor
// (DECISIONS_LOG_PHASE_2.md ADR-030) to resume a grace period for its
// correctly-computed remaining duration after a failover, instead of
// restarting the full 60s from scratch.
func (m *Manager) startAbandonTimerWithDuration(gameID string, color store.Color, d time.Duration) {
	key := abandonKey(gameID, color)
	m.armTimer(key, d, func() {
		m.onAbandonTimeout(gameID, color)
	})
}

// remainingAbandonDuration computes how much of the abandonment grace
// period is left, given when the disconnect was recorded. Pulled out as a
// pure function (no receiver, no side effects) specifically so it's
// unit-testable without needing to inspect a *time.Timer's internal state,
// which Go's stdlib does not expose — see abandon_test.go.
func remainingAbandonDuration(disconnectedAt time.Time) time.Duration {
	remaining := abandonTimeout - time.Since(disconnectedAt)
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// armAbandonTimerForColor resumes an abandonment grace period for color from
// a persisted disconnect timestamp (DECISIONS_LOG_PHASE_2.md ADR-030).
// Manager.abandonTimers is pure per-process memory — a session constructed
// fresh via hydrateGameSession (Phase 2 failover) or restoreGame (Phase 1
// startup) has no such timer running even if a disconnect happened before
// this process existed. disconnectedAt is nil when the player is not
// currently in a disconnect grace period (never disconnected, or already
// reconnected and cleared) — a no-op in that case.
//
// Always uses time.AfterFunc, even when the grace period has already fully
// elapsed (remaining <= 0), rather than calling onAbandonTimeout inline:
// this function can run before the caller has necessarily finished
// registering the session into GameRegistry (see hydrateGameSession's
// caller sites, which register only after hydrateFn returns), and
// onAbandonTimeout looks the session up via registry.Get. Scheduling
// asynchronously — even at duration 0 — gives the caller's registration a
// chance to complete first in the overwhelmingly common case, and
// onAbandonTimeout's existing Transition-based idempotency (session already
// terminal → no-op, logged at Debug) makes this safe even in the rare case
// it doesn't.
func (m *Manager) armAbandonTimerForColor(gameID string, color store.Color, disconnectedAt *time.Time) {
	if disconnectedAt == nil {
		return
	}
	m.startAbandonTimerWithDuration(gameID, color, remainingAbandonDuration(*disconnectedAt))
}

// effectiveDisconnectedAt resolves the ambiguity DECISIONS_LOG_PHASE_2.md
// ADR-031 identified in ADR-030's original design: persisted is nil in two
// genuinely different situations that armAbandonTimerForColor cannot tell
// apart on its own — "this player is definitely fine" (true in ordinary
// steady state) and "we have no idea, because the process that would have
// recorded a disconnect died before it could" (true only at hydration time,
// for a game whose owning instance crashed while both players were still
// connected — HandleDisconnect never got the chance to run for either
// color, so neither timestamp was ever written).
//
// A freshly-hydrated GameSession always starts with both connection slots
// empty regardless of which of these two situations actually holds, so
// there is no way to distinguish them from the session's own state either.
// The only safe default for an ACTIVE or WAITING_FOR_PLAYER game is to
// treat "we have no idea" as "assume disconnected as of right now" — a
// player who is in fact still fine self-cancels this defensively-armed
// timer within their own HandleConnect call moments later (same mechanism
// that already cancels a real, individually-observed disconnect timer), so
// the cost of guessing wrong in the safe direction is bounded to a few
// seconds of an unnecessary timer that immediately gets cancelled, not a
// false abandonment.
//
// Only ever called at hydration (armAbandonTimersForGame, restoreGame) —
// never from HandleDisconnect itself, which always has a real, individually
// observed timestamp to write and never needs this fallback.
func effectiveDisconnectedAt(status store.GameStatus, persisted *time.Time) *time.Time {
	if persisted != nil {
		return persisted // real, individually-observed disconnect — trust as-is
	}
	if status == store.GameStatusActive || status == store.GameStatusWaiting {
		now := time.Now()
		return &now // unknown at hydration time — assume disconnected as of now, not "fine"
	}
	return nil
}

// armAbandonTimersForGame is DELIBERATELY REMOVED as of DECISIONS_LOG_PHASE_3.md
// ADR-041 — see armTimersForGame and armTimersForGameStatus below, which
// replace it at all three former call sites (HandleConnect, ResolveGame,
// restoreGame). This comment intentionally left as a pointer for anyone
// grepping for the old name; remove once this has been in the codebase a
// while and the pointer no longer earns its keep.

// effectiveWindowStartedAt mirrors effectiveDisconnectedAt's exact reasoning
// (DECISIONS_LOG_PHASE_2.md ADR-031) for DECISIONS_LOG_PHASE_3.md ADR-041's
// first-move grace period, with one difference worth being explicit about:
// unlike effectiveDisconnectedAt, a nil persisted value here is NEVER the
// "steady state and fine" case — games.activated_at is set atomically with
// the WAITING→ACTIVE transition itself (GameStore.ActivateGame), so by the
// time this is ever called (game is ACTIVE), a nil value always means that
// specific write's activated_at portion failed for this specific game, not
// "hasn't happened yet in the ordinary flow." Same safe-direction default
// regardless: assume the window started now, giving the hydrating instance a
// full fresh window rather than guessing a shorter one — a player still
// genuinely inside a real window loses nothing (the window merely resets to
// full length), which is at most as generous as intended, never less.
func effectiveWindowStartedAt(persisted *time.Time) time.Time {
	if persisted != nil {
		return *persisted
	}
	return time.Now()
}

// remainingFirstMoveWindowDuration computes how much of
// DECISIONS_LOG_PHASE_3.md ADR-041's first-move grace period remains, given
// when the current window actually started. Mirrors remainingAbandonDuration's
// shape and reasoning exactly — pulled out as a pure function for the same
// testability reason.
func remainingFirstMoveWindowDuration(windowStartedAt time.Time) time.Duration {
	remaining := firstMoveTimeout - time.Since(windowStartedAt)
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// armTimersForGameStatus arms whichever grace-period timer(s) currently
// apply to game, given its persisted status and (only needed/fetched when
// ACTIVE) move history — DECISIONS_LOG_PHASE_3.md ADR-041's gating logic,
// shared by restoreGame (which already has both values in scope from its own
// board-reconstruction work) and armTimersForGame below (which fetches them
// fresh for call sites that don't).
//
// Per ADR-041's own Consequences, armAbandonTimerForColor's internals do not
// change — only whether/when each of the three former
// armAbandonTimersForGame call sites now routes through here instead of
// arming the ordinary per-color timers unconditionally.
func (m *Manager) armTimersForGameStatus(game *store.Game, moves []*store.Move) {
	if game.Status != store.GameStatusActive || len(moves) >= 2 {
		// WAITING (or a terminal status, always a no-op via
		// effectiveDisconnectedAt either way): existing behavior, unchanged.
		// ACTIVE with the first-move mechanism already retired (Black's reply
		// landed): also existing behavior, unchanged — identical to any other
		// ACTIVE game from this function's perspective.
		m.armAbandonTimerForColor(game.ID, store.ColorWhite, effectiveDisconnectedAt(game.Status, game.WhiteDisconnectedAt))
		m.armAbandonTimerForColor(game.ID, store.ColorBlack, effectiveDisconnectedAt(game.Status, game.BlackDisconnectedAt))
		return
	}

	// ACTIVE with fewer than 2 moves played: DECISIONS_LOG_PHASE_3.md ADR-041
	// sub-decision 2 — the ordinary per-color timers are not armed at all
	// during this window; the first-move timer is the sole governing
	// mechanism.
	var windowStartedAt time.Time
	if len(moves) == 0 {
		// White's window: anchored on activation.
		windowStartedAt = effectiveWindowStartedAt(game.ActivatedAt)
	} else {
		// Black's window: anchored on White's move-persist time — no fallback
		// needed, moves.played_at is always populated (MoveStore.SaveMove's
		// RETURNING clause).
		windowStartedAt = moves[0].PlayedAt
	}

	m.armFirstMoveTimer(game.ID, remainingFirstMoveWindowDuration(windowStartedAt))
}

// armTimersForGame re-fetches gameID's row (and, only when ACTIVE, its move
// history) and arms whichever grace-period timer(s) currently apply — see
// armTimersForGameStatus for the actual gating logic. Replaces the narrower,
// pre-ADR-041 armAbandonTimersForGame at both of its remaining call sites
// that don't already have game/moves in scope (HandleConnect's registry-miss
// fallback, ResolveGame's claim-and-hydrate branch); restoreGame calls
// armTimersForGameStatus directly, since it already has both values loaded
// from its own board-reconstruction work and a second GetGame/
// GetMovesForGame round-trip there would be pure waste.
//
// Costs up to two extra reads (GetGame, and GetMovesForGame only when the
// game is ACTIVE) — same "infrequent hydration moment, not a hot path" bound
// the superseded armAbandonTimersForGame already documented.
func (m *Manager) armTimersForGame(ctx context.Context, gameID string) {
	game, err := m.gameStore.GetGame(ctx, gameID)
	if err != nil {
		slog.Error("armTimersForGame: failed to read game for timer resumption",
			"gameID", gameID, "error", err)
		return
	}

	var moves []*store.Move
	if game.Status == store.GameStatusActive {
		moves, err = m.moveStore.GetMovesForGame(ctx, gameID)
		if err != nil {
			slog.Error("armTimersForGame: failed to read moves for timer resumption",
				"gameID", gameID, "error", err)
			return
		}
	}

	m.armTimersForGameStatus(game, moves)
}

func (m *Manager) cancelAbandonTimer(gameID string, color store.Color) {
	m.cancelTimer(abandonKey(gameID, color))
}

// firstMoveTimerKey returns the map key for a game's DECISIONS_LOG_PHASE_3.md
// ADR-041 first-move grace-period timer. Never collides with abandonKey's
// shape: abandonKey's second segment is always a real store.Color
// ("WHITE"/"BLACK"); "FIRSTMOVE" is not.
func firstMoveTimerKey(gameID string) string {
	return gameID + ":FIRSTMOVE"
}

func (m *Manager) armFirstMoveTimer(gameID string, d time.Duration) {
	m.armTimer(firstMoveTimerKey(gameID), d, func() {
		m.onFirstMoveTimeout(gameID)
	})
}

func (m *Manager) cancelFirstMoveTimer(gameID string) {
	m.cancelTimer(firstMoveTimerKey(gameID))
}

// opponentNeverConnectedTimerKey returns the map key for a matched game's
// DECISIONS_LOG_PHASE_3.md ADR-042 opponent-connect timeout. Same
// collision-safety reasoning as firstMoveTimerKey.
func opponentNeverConnectedTimerKey(gameID string) string {
	return gameID + ":OPPONENT_NEVER_CONNECTED"
}

// armOpponentNeverConnectedTimer arms DECISIONS_LOG_PHASE_3.md ADR-042's
// timeout. Called by Manager.CreateMatchedGame (Phase 3 Step 3) at the
// moment a matched game is created — always the full matchedOpponentConnectTimeout,
// never a partial/resumed duration: unlike the first-move timer or the
// ordinary abandon timer, this one has no hydration-time resumption path,
// since it only ever needs to survive from game-creation to the opponent's
// first connect, both of which happen on the instance that created the game
// (Manager.CreateMatchedGame registers the session locally in the same call
// that arms this timer — no failover-survival gap to close here the way
// ADR-030/ADR-031 closed one for the ordinary abandon timer).
func (m *Manager) armOpponentNeverConnectedTimer(gameID string) {
	m.armTimer(opponentNeverConnectedTimerKey(gameID), matchedOpponentConnectTimeout, func() {
		m.onOpponentNeverConnectedTimeout(gameID)
	})
}

func (m *Manager) cancelOpponentNeverConnectedTimer(gameID string) {
	m.cancelTimer(opponentNeverConnectedTimerKey(gameID))
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
