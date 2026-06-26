package chess

import "errors"

// ErrIllegalMove is returned when a submitted move is not legal in the current position.
// This includes moves that are syntactically valid SAN but illegal (wrong piece, blocked
// path, leaves king in check, wrong turn).
var ErrIllegalMove = errors.New("illegal move")

// ErrInvalidFEN is returned when a FEN string cannot be parsed into a valid position.
var ErrInvalidFEN = errors.New("invalid FEN")