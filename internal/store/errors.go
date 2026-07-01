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
)