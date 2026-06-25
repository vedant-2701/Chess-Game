package auth

import "errors"

// Sentinel errors returned by VerifyPlayerToken.
// Callers use errors.Is() to distinguish expiry (recoverable with a new token)
// from invalid (reject the connection outright).
var (
	ErrTokenExpired = errors.New("token is expired")
	ErrTokenInvalid = errors.New("token is invalid")
)