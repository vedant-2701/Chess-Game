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