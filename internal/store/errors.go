package store

import "errors"

// Sentinel errors returned by store methods.
// Callers use errors.Is() to distinguish "not found" from infrastructure failures.
var (
	ErrGameNotFound = errors.New("game not found")
	ErrUserNotFound = errors.New("user not found")
)