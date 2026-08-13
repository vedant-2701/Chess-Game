package game

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vedant-2701/chess/internal/store"
)

// StartHeartbeat launches a background goroutine (PHASE_2.md Step 6) that
// periodically renews this instance's ownership claims for every locally-
// active game plus its own liveness key, keeping both comfortably inside
// their respective TTLs (DECISIONS_LOG_PHASE_2.md ADR-023) for as long as
// this instance is actually alive and serving.
//
// Ticks at LivenessRenewInterval (3s) via a single ticker — deliberately not
// two independently-scheduled tickers at LivenessRenewInterval and
// OwnershipRenewInterval. Renewing ownership more often than
// OwnershipRenewInterval's 10s "must renew by" bound is harmless
// (RenewOwnershipBatch's compare-and-swap just resets the same 30s TTL
// early); ADR-023's Consequences section is explicit that the ticker
// performs exactly two Redis writes per tick, which is only true under a
// single shared ticker, not two independent ones.
//
// Calls SetAlive synchronously once, before starting the ticker — a failure
// here is returned immediately rather than silently starting a ticker for an
// instance that was never able to establish its own liveness key in the
// first place.
//
// Returns a stop function. Calling it stops the ticker goroutine AND
// proactively releases every game this instance currently owns plus its own
// liveness key (PHASE_2.md Scope: "Graceful-shutdown release of an
// instance's own Redis directory entries... don't make a deliberate
// scale-down wait out a TTL"). stop blocks until the ticker goroutine has
// fully exited before performing the release, so an in-flight renewal tick
// can never race the release for the same key. stop must be called at most
// once.
func (m *Manager) StartHeartbeat(ctx context.Context) (stop func(), err error) {
	if m.directory == nil {
		return nil, fmt.Errorf("Manager.StartHeartbeat: %w", ErrDirectoryNotConfigured)
	}

	if setAliveErr := m.directory.SetAlive(ctx, m.instanceID); setAliveErr != nil {
		return nil, fmt.Errorf("Manager.StartHeartbeat instanceID=%s: %w", m.instanceID, setAliveErr)
	}

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(LivenessRenewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				m.heartbeatTick(ctx)
			}
		}
	}()

	stop = func() {
		close(done)
		<-stopped // wait for any in-flight tick to finish before releasing.
		m.releaseHeartbeatEntries(ctx)
	}
	return stop, nil
}

// heartbeatTick performs one heartbeat cycle: renew this instance's liveness
// key, then batch-renew ownership for every locally-active game. Both use
// ctx directly (not detached) — unlike releaseHeartbeatEntries below, an
// ordinary renewal tick is NOT meant to survive the parent context being
// cancelled; once shutdown begins, ticks should stop reflecting renewal, and
// the explicit, detached release path (triggered by stop()) is what's
// supposed to run instead.
func (m *Manager) heartbeatTick(ctx context.Context) {
	if err := m.directory.RenewAlive(ctx, m.instanceID); err != nil {
		slog.Error("heartbeat: RenewAlive failed", "instanceID", m.instanceID, "error", err)
	}

	sessions := m.registry.AllActive()
	if len(sessions) == 0 {
		return
	}
	gameIDs := make([]string, len(sessions))
	for i, s := range sessions {
		gameIDs[i] = s.ID
	}

	renewed, err := m.directory.RenewOwnershipBatch(ctx, gameIDs, m.instanceID)
	if err != nil {
		slog.Error("heartbeat: RenewOwnershipBatch failed",
			"instanceID", m.instanceID, "count", len(gameIDs), "error", err)
		return
	}
	for gameID, ok := range renewed {
		if !ok {
			// Expected, not necessarily alarming: this instance no longer
			// legitimately owns gameID, e.g. it was taken over elsewhere
			// during TD-P2-001's residual false-positive window (ADR-023).
			// Logged at Warn, not Error, per exactly that reasoning.
			slog.Warn("heartbeat: lost ownership renewal for game — no longer the recorded owner",
				"gameID", gameID, "instanceID", m.instanceID)
		}
	}

	m.renewActiveGameMarkers(ctx, sessions)
}

// renewActiveGameMarkers batch-renews the matchmaking_active_game:{userID}
// marker (DECISIONS_LOG_PHASE_3.md ADR-037) for every player in every
// locally-active game, in the same tick that renews ownership. Best-effort,
// same as ownership renewal above — a failure here is logged, never
// propagated; the marker is a convenience cache for matchmaking-service's
// check-before-enqueue logic, not a source of truth for anything (Postgres
// is), so its failure mode is bounded staleness, not a correctness loss.
//
// Re-signs a fresh PlayerClaims token for each player on every tick rather
// than reusing whatever token CreateGame/JoinGame/CreateMatchedGame
// originally minted: PlayerClaims tokens are fully self-contained and
// deterministically re-mintable from (gameID, userID, color, secret) at any
// time — any correctly-signed, unexpired token is equally valid, so there is
// no need to thread the ORIGINAL token bytes through GameSession just to
// republish them unchanged. This also sidesteps a real structural problem:
// JoinGame deliberately never touches a live GameSession (it may run on a
// different instance than the one hosting it — see JoinGame's own doc
// comment), so a GameSession-held-token design would have no way to
// populate Black's token for renewal purposes in that case at all, only
// White's.
func (m *Manager) renewActiveGameMarkers(ctx context.Context, sessions []*GameSession) {
	markers := make(map[string]ActiveGameMarker, len(sessions)*2)
	for _, session := range sessions {
		snap := session.CurrentStateSnapshot()
		if snap.Status != store.GameStatusWaiting && snap.Status != store.GameStatusActive {
			// A session can still be briefly registered in a terminal status
			// between Transition succeeding and finalizeGame's Unregister
			// running (the same narrow window ADR-030's own callers already
			// tolerate elsewhere in this file) — skip it; a terminal game has
			// no business holding either player in "has an active game" state.
			continue
		}

		whiteToken, err := m.signToken(session.ID, snap.PlayerWhiteID, string(store.ColorWhite))
		if err != nil {
			slog.Error("heartbeat: failed to sign token for active-game marker renewal",
				"gameID", session.ID, "userID", snap.PlayerWhiteID, "color", "WHITE", "error", err)
		} else {
			markers[snap.PlayerWhiteID] = ActiveGameMarker{
				GameID:        session.ID,
				ConnectToken:  whiteToken,
				InstanceLabel: m.instanceID,
				WSPath:        "/connect/" + m.instanceID,
			}
		}

		if snap.PlayerBlackID == "" {
			continue // WAITING_FOR_PLAYER shared-link game, nobody has joined yet.
		}
		blackToken, err := m.signToken(session.ID, snap.PlayerBlackID, string(store.ColorBlack))
		if err != nil {
			slog.Error("heartbeat: failed to sign token for active-game marker renewal",
				"gameID", session.ID, "userID", snap.PlayerBlackID, "color", "BLACK", "error", err)
		} else {
			markers[snap.PlayerBlackID] = ActiveGameMarker{
				GameID:        session.ID,
				ConnectToken:  blackToken,
				InstanceLabel: m.instanceID,
				WSPath:        "/connect/" + m.instanceID,
			}
		}
	}

	if len(markers) == 0 {
		return
	}
	if err := m.directory.RenewActiveGameMarkersBatch(ctx, markers); err != nil {
		slog.Error("heartbeat: RenewActiveGameMarkersBatch failed",
			"instanceID", m.instanceID, "count", len(markers), "error", err)
	}
}

// releaseHeartbeatEntries is StartHeartbeat's stop-time cleanup: release
// every currently-registered game's ownership plus this instance's own
// liveness key.
//
// Uses a context detached from ctx's cancellation (the same
// context.WithTimeout(context.WithoutCancel(ctx), ...) pattern established
// by DECISIONS_LOG_PHASE_1.md ADR-019 and reinforced by
// DECISIONS_LOG_PHASE_2.md ADR-027): ctx here is almost certainly the
// server-lifetime context that is itself being cancelled as part of the same
// shutdown sequence this cleanup runs during (ADR-018) — this is exactly the
// one operation in the whole shutdown path that must survive that
// cancellation to have any effect at all. Bounded independently (5s) so a
// genuinely stuck Redis call cannot hang shutdown indefinitely.
func (m *Manager) releaseHeartbeatEntries(ctx context.Context) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	sessions := m.registry.AllActive()
	for _, session := range sessions {
		if err := m.directory.ReleaseOwnership(releaseCtx, session.ID, m.instanceID); err != nil {
			slog.Error("heartbeat shutdown: ReleaseOwnership failed",
				"gameID", session.ID, "instanceID", m.instanceID, "error", err)
		}
	}
	if err := m.directory.ReleaseAlive(releaseCtx, m.instanceID); err != nil {
		slog.Error("heartbeat shutdown: ReleaseAlive failed", "instanceID", m.instanceID, "error", err)
	}
	slog.Info("heartbeat: released directory entries on shutdown",
		"instanceID", m.instanceID, "gamesReleased", len(sessions))
}
