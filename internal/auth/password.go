package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// --- PHASE_3.md addendum: username/password login (fixes TD-P3-007) -------
//
// matchmaking-service never touches Postgres (ARCHITECTURE.md's
// Matchmaking section), so POST /matchmaking/queue cannot create a users
// row the way POST /games / POST /games/:id/join do (both call
// userStore.CreateOrGetUser). A userID that has never gone through
// POST /games at least once therefore has no row at all, and
// CreateMatchedGame's FK constraint on users then fails deterministically
// for it — indistinguishable, from the outside, from a genuine
// CreateMatchedGame failure. These endpoints (internal/api/auth_handler.go)
// let a client obtain, or later recover, a stable userID without going
// through game creation first.
//
// Deliberately minimal by explicit instruction: the endpoints return the
// userID directly, with NO issued token, NO session, and NO expiration —
// the client just remembers the userID and passes it to every existing
// endpoint exactly as it always has. This does not introduce or change any
// existing trust boundary: every endpoint already accepted a client-
// supplied userID at face value before this addition (CreateGame/JoinGame
// never verified that the caller "owns" a given anonymous UUID either) —
// this only offers an alternative way to obtain one. Token-based sessions
// with expiration are an explicitly deferred future addition per the
// person's own instruction, not an oversight.
//
// bcrypt (not a faster/lighter hash) is non-negotiable even at this
// minimal scope — storing passwords at all obligates a slow, salted,
// adaptive hash. "Keep it simple" describes the endpoint surface (no
// tokens, no sessions), not license to weaken how the password itself is
// stored; those are independent axes and only the first one was the
// person's actual instruction.

// bcryptCost uses bcrypt's own recommended default rather than a custom
// value — no product requirement here calls for tuning it away from that
// default in either direction.
const bcryptCost = bcrypt.DefaultCost

// HashPassword returns a bcrypt hash of the given plaintext password,
// suitable for storing in users.password_hash. Never returns the plaintext
// in the error path either — GenerateFromPassword's own error cases
// (password too long — bcrypt's 72-byte input limit) don't echo it back.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth.HashPassword: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword reports whether password matches the given bcrypt hash.
// Returns (false, nil) for a normal mismatch — NOT an error; only a
// genuine infrastructure/format problem (e.g. a corrupted stored hash)
// returns a non-nil error, which callers should treat as a 500, not an
// authentication failure.
//
// Callers (internal/api's AuthHandler.Login) must give a false result the
// identical response shape as an unknown-username result — this function
// does not do that itself, since distinguishing the two here would just
// move the username-enumeration side channel one layer down rather than
// closing it; this function's only job is to report what is actually true.
func VerifyPassword(password, hash string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return false, fmt.Errorf("auth.VerifyPassword: %w", err)
}
