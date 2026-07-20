package game

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/vedant-2701/chess/internal/auth"
	internalchess "github.com/vedant-2701/chess/internal/chess"
	"github.com/vedant-2701/chess/internal/store"
)

// ResolveGame implements PHASE_2.md's resolve-then-connect flow (ADR-022):
// given a gameID and the identity of the requesting player (already
// authenticated via PlayerClaims at the HTTP layer — see
// internal/api/game_handler.go's Resolve handler), determine which instance
// currently owns the game, claiming/hydrating locally if this instance is
// about to become the owner, and mint a short-lived ConnectClaims scoped to
// that instance.
//
// Algorithm (PHASE_2.md Connection Flow step 3):
//  1. Read the ownership record for gameID.
//  2. If an owner is recorded AND that owner's liveness key is present,
//     the owner is confirmed alive — mint ConnectClaims naming it, no local
//     work needed.
//  3. Otherwise (no owner recorded, or the recorded owner's liveness key is
//     absent — confirmed dead even if its 30s ownership TTL hasn't lapsed
//     yet): atomically claim ownership for THIS instance. expectedPriorOwner
//     is "" for a genuine first claim, or the (confirmed-dead) owner's ID for
//     a takeover — both collapse into ClaimOwnership's single compare-and-
//     swap primitive (see directory.go's doc comment).
//  4. If the claim succeeds: hydrate (or confirm already-hydrated) the
//     session locally via GetOrHydrate, then mint ConnectClaims naming this
//     instance.
//  5. If the claim is lost (another instance won the race between this
//     instance's read and its claim attempt): re-read the ownership record
//     once and defer to whoever won.
func (m *Manager) ResolveGame(ctx context.Context, gameID, userID string, color store.Color) (connectToken, instanceLabel string, err error) {
	if m.directory == nil {
		// Deliberately not a panic (CODING_GUIDELINES.md §1): directory=nil is
		// a valid, documented NewManager configuration for callers that never
		// use ResolveGame (see NewManager's doc comment). Reaching this means
		// a caller invoked ResolveGame on a Manager that was never wired for
		// it — a real bug, but one that should surface as a normal error the
		// caller can log and return a 500 for, not crash the process.
		return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: %w", gameID, ErrDirectoryNotConfigured)
	}

	owner, ok, err := m.directory.GetOwner(ctx, gameID)
	if err != nil {
		return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: GetOwner: %w", gameID, err)
	}

	if ok {
		alive, aliveErr := m.directory.IsAlive(ctx, owner)
		if aliveErr != nil {
			return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: IsAlive owner=%s: %w", gameID, owner, aliveErr)
		}
		if alive {
			instanceLabel = owner
		}
	}

	if instanceLabel == "" {
		expectedPriorOwner := ""
		if ok {
			expectedPriorOwner = owner // takeover from a confirmed-dead owner
		}

		claimed, claimErr := m.directory.ClaimOwnership(ctx, gameID, m.instanceID, expectedPriorOwner)
		if claimErr != nil {
			return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: ClaimOwnership: %w", gameID, claimErr)
		}

		if !claimed {
			// Lost the race — another instance claimed it between this
			// call's GetOwner/IsAlive check and the ClaimOwnership attempt
			// above. Defer to whoever won rather than retrying the claim.
			newOwner, newOk, getErr := m.directory.GetOwner(ctx, gameID)
			if getErr != nil {
				return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: GetOwner after lost claim: %w", gameID, getErr)
			}
			if !newOk {
				return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: %w", gameID, ErrResolveFailed)
			}
			instanceLabel = newOwner
		} else {
			instanceLabel = m.instanceID

			// This instance now legitimately owns gameID in the directory —
			// hydrate the local session (GetOrHydrate is a no-op if it's
			// already registered, e.g. a second racing resolve call for a
			// game this instance already owned). See hydrateGameSession's
			// doc comment for why this must handle terminal-status games
			// too, unlike RestoreActiveGames' startup-only restoreGame.
			//
			// Not attempting to release the claim on hydration failure: the
			// only realistic cause is store.ErrGameNotFound for a gameID
			// that was never actually created, which should be unreachable
			// in practice (gameIDs only ever come from a real CreateGame
			// response or a shared link derived from one — a PlayerClaims
			// token for a fabricated gameID would already have failed
			// signature verification at the HTTP layer before reaching
			// here). If it somehow does happen, the claim simply expires on
			// its own via OwnershipTTL — no heartbeat ticker will renew a
			// claim for a session that was never registered, so this is a
			// bounded, self-healing failure mode, not a permanent leak.
			if _, hydrateErr := m.registry.GetOrHydrate(ctx, gameID, func(hydrateCtx context.Context) (*GameSession, error) {
				return m.hydrateGameSession(hydrateCtx, gameID)
			}); hydrateErr != nil {
				return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: hydrate: %w", gameID, hydrateErr)
			}
		}
	}

	claims := auth.ConnectClaims{
		GameID:        gameID,
		UserID:        userID,
		Color:         string(color),
		InstanceLabel: instanceLabel,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(auth.ConnectClaimsTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token, err := auth.SignConnectToken(claims, m.jwtSecret)
	if err != nil {
		return "", "", fmt.Errorf("Manager.ResolveGame gameID=%s: %w", gameID, err)
	}

	return token, instanceLabel, nil
}

// hydrateGameSession reconstructs a GameSession for gameID from the database,
// for use as GameRegistry.GetOrHydrate's hydrateFn (PHASE_2.md Step 3/5).
//
// Unlike restoreGame (used exclusively at startup, against ACTIVE/WAITING
// rows already filtered by GetActiveGames), this must succeed for ANY game
// status, including COMPLETED/ABANDONED. PHASE_2.md's Step 5/11 checklist
// explicitly requires that resolving/reconnecting to an already-terminal
// game succeeds and delivers correct final GAME_STATE — a case Phase 1 never
// had to handle at all: finalizeGame always unregistered a terminal session
// immediately, and there was no hydrate-on-miss path that could bring one
// back. Phase 2's resolve-then-connect flow introduces the first legitimate
// reason a terminal game's session might need to exist again after the fact
// (a player who was disconnected when the game ended, reconnecting to see
// the result).
//
// TD-P2-004 (new, flagged not solved): a session hydrated here for a
// terminal game remains registered indefinitely once created — nothing
// currently re-unregisters it, since finalizeGame's normal triggers (a new
// MOVE/RESIGN/timeout/abandon-timer firing) can never occur again for an
// already-terminal game. This reopens, in a narrow and infrequent way, the
// unbounded-registry-growth concern finalizeGame was originally built to
// close (see finalizeGame's own doc comment) — narrow because it only
// affects games someone deliberately reconnects to after completion, not
// every completed game. Not solved here: no eviction/TTL mechanism for
// registered sessions exists yet anywhere in this codebase, and building one
// for this specific case would be exactly the kind of ahead-of-demonstrated-
// need machinery this project has repeatedly declined to build early
// (ADR-014, ADR-016, ADR-023's TD-P2-001). Worth revisiting if it's ever
// shown to matter in practice.
func (m *Manager) hydrateGameSession(ctx context.Context, gameID string) (*GameSession, error) {
	g, err := m.gameStore.GetGame(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("hydrateGameSession gameID=%s: get game: %w", gameID, err)
	}

	moves, err := m.moveStore.GetMovesForGame(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("hydrateGameSession gameID=%s: get moves: %w", gameID, err)
	}
	sans := make([]string, len(moves))
	for i, mv := range moves {
		sans[i] = mv.SAN
	}

	board, err := internalchess.GameFromMoves(sans)
	if err != nil {
		return nil, fmt.Errorf("hydrateGameSession gameID=%s: replay moves: %w", gameID, err)
	}

	// Zombie check — only meaningful when the DB still claims ACTIVE but the
	// board has actually reached a terminal position (mirrors restoreGame's
	// identical check). Deliberately gated on g.Status == ACTIVE, NOT on
	// "board says ended" alone: a genuinely COMPLETED-by-resignation/
	// timeout/abandonment game's board will often never reach a
	// checkmate/stalemate position at all, and its DB status is already
	// correct — DetectOutcome returning ended=true in that case is the
	// normal, expected outcome for a real checkmate game whose status
	// already correctly says COMPLETED, not a sign anything needs fixing.
	if g.Status == store.GameStatusActive {
		if outcome, ended := m.validator.DetectOutcome(board); ended {
			slog.Warn("hydrateGameSession: zombie ACTIVE game — correcting DB status",
				"gameID", gameID, "outcome", outcome.Winner, "reason", outcome.Reason)
			storeOutcome := store.Outcome(outcome.Winner)
			storeReason := store.OutcomeReason(outcome.Reason)
			if dbErr := m.gameStore.UpdateGameStatus(ctx, gameID, store.GameStatusActive, store.GameStatusCompleted, &store.GameOutcome{
				Outcome: storeOutcome,
				Reason:  storeReason,
			}); dbErr != nil {
				slog.Error("hydrateGameSession: failed to correct zombie ACTIVE in DB",
					"gameID", gameID, "error", dbErr)
				// Fall through anyway — construct the session with the DB's
				// stale ACTIVE status rather than failing hydration
				// outright. A later terminal-state transition attempt will
				// surface the conflict via UpdateGameStatus's fromStatus
				// predicate if it ever matters.
			} else {
				g.Status = store.GameStatusCompleted
				g.Outcome = &storeOutcome
				g.OutcomeReason = &storeReason
			}
		}
	}

	session := NewGameSessionFromDB(g, board)
	m.setClockTimeoutCallback(session)

	// Only live (non-terminal) games need EventBus forwarding. A terminal
	// game will never publish another GAME_OVER, so subscribing here would
	// leak startEventSubscriber's goroutine forever — it only exits on a
	// GAME_OVER event or the channel closing, and neither will ever happen
	// for an already-finished game. This is exactly why restoreGame only
	// ever subscribes for the ACTIVE/WAITING rows GetActiveGames returns;
	// this function must apply the same gate explicitly, since (unlike
	// restoreGame) it can be called against a genuinely terminal row.
	if g.Status == store.GameStatusWaiting || g.Status == store.GameStatusActive {
		ch, unsubscribe, subErr := m.eventBus.Subscribe(ctx, gameID)
		if subErr != nil {
			return nil, fmt.Errorf("hydrateGameSession gameID=%s: subscribe: %w", gameID, subErr)
		}
		m.startEventSubscriber(session, ch, unsubscribe)
	}

	slog.Info("game hydrated on resolve", "gameID", gameID, "status", g.Status)
	return session, nil
}
