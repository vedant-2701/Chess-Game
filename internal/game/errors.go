package game

import (
	"errors"
	"fmt"
)

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

var (
	// ErrGameNotJoinable is returned by Manager.JoinGame when the game exists
	// but is not in WAITING_FOR_PLAYER status. The API handler maps this to
	// HTTP 409 Conflict.
	ErrGameNotJoinable = errors.New("game is not joinable")

	// ErrSelfPlay is returned by Manager.JoinGame when the joining userID
	// matches the game creator (White). The API handler maps this to HTTP 409
	// Conflict.
	ErrSelfPlay = errors.New("cannot join your own game")

	// ErrResolveFailed is returned by Manager.ResolveGame (PHASE_2.md Step 5)
	// in the astronomically unlikely case where this instance loses a fresh
	// ClaimOwnership race AND the immediate re-check GetOwner also finds no
	// owner recorded (e.g. the winner's own claim already expired/was
	// released in the tiny window between the two calls). Not expected to be
	// hit in practice; exists so ResolveGame has a defined error instead of
	// a nil/zero-value instanceLabel silently propagating.
	ErrResolveFailed = errors.New("failed to resolve game to an owning instance")

	// ErrDirectoryNotConfigured is returned by Manager.ResolveGame and
	// Manager.StartHeartbeat when called on a Manager constructed with
	// directory=nil (a valid configuration for any caller that never uses
	// either — see NewManager's doc comment). Reaching this means a caller
	// bug: one of these methods was invoked on a Manager that was never wired
	// with a RoutingDirectory. Shared between both methods rather than given
	// separate sentinels, since they share the exact same precondition.
	ErrDirectoryNotConfigured = errors.New("manager is not configured with a routing directory")
)

// MoveRejectionError is returned by MoveProcessor.ProcessMove when a move is
// rejected for a client-attributable reason. The Manager constructs and sends a
// MOVE_REJECTED message to the submitting player using the Reason field.
//
// Callers distinguish client rejections from infrastructure failures with errors.As:
//
//	var rejection *MoveRejectionError
//	if errors.As(err, &rejection) {
//	    // send MOVE_REJECTED with rejection.Reason
//	} else {
//	    // internal error — log and send ERROR to client
//	}
type MoveRejectionError struct {
	// Reason is one of the RejectReason* constants defined in messages.go.
	Reason string
}

func (e *MoveRejectionError) Error() string {
	return fmt.Sprintf("move rejected: %s", e.Reason)
}