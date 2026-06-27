package game

import "errors"

var (
	// ErrGameNotFound is returned by GameRegistry.Get when no session exists
	// for the requested gameID. Distinct from store.ErrGameNotFound which
	// signals a missing database record.
	ErrGameNotFound = errors.New("game session not found")

	// ErrConnectionOccupied is returned by GameSession.RegisterConnection when
	// the color slot already holds a live connection. Callers should switch to
	// ReplaceConnection for reconnection.
	ErrConnectionOccupied = errors.New("connection slot already occupied")

	// ErrInvalidTransition is returned by GameSession.Transition when the
	// requested state change is not permitted by the game state machine.
	ErrInvalidTransition = errors.New("invalid state transition")
)