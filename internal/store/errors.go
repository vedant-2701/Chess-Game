package store

import "errors"

// Sentinel errors returned by store methods.
// Callers use errors.Is() to distinguish "not found" from infrastructure failures.
var (
	ErrGameNotFound = errors.New("game not found")
	ErrUserNotFound = errors.New("user not found")

	// ErrGameNotJoinable is returned by UpdatePlayerBlack when its conditional
	// UPDATE (id match, status = WAITING_FOR_PLAYER, player_black_id IS NULL)
	// affects zero rows because the row exists but the predicate failed — i.e.
	// the game already has a Black player or is no longer WAITING_FOR_PLAYER.
	// Distinct from ErrGameNotFound, which means the row does not exist at all.
	// This is the storage-layer half of the atomic join guarantee: internal/game
	// must not rely on a read-then-write check for this invariant (see ADR-016).
	ErrGameNotJoinable = errors.New("game is not joinable")

	// ErrGameStatusConflict is returned by UpdateGameStatus when its conditional
	// UPDATE (id match, status = fromStatus) affects zero rows because the row's
	// current status no longer equals the caller's expected fromStatus. Same
	// discipline as ErrGameNotJoinable/ADR-016: the UPDATE's WHERE predicate,
	// evaluated atomically by PostgreSQL, is the actual correctness guarantee
	// against two concurrent writers racing a terminal-state transition for the
	// same game — including, under Phase 2, two different instances that each
	// believe they legitimately own the game during TD-P2-001's residual
	// liveness-false-positive window (see DECISIONS_LOG_PHASE_2.md ADR-021,
	// ADR-023). Distinct from ErrGameNotFound: no current call site can ever
	// observe a missing row here, since every caller already established the
	// row's existence (a prior GetGame/GetActiveGames read, or an in-memory
	// GameSession that was itself hydrated from one) before calling.
	ErrGameStatusConflict = errors.New("game status conflict: expected status no longer matches")
)