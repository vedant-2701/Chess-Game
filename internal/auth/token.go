package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PlayerClaims are the JWT claims embedded in every player token.
//
// Color is string, not store.Color, to keep the auth package free of store
// dependencies. The game layer validates the color value after verification.
//
// GameID and UserID are included so the WebSocket handler can extract game
// context from the token alone, without a database lookup on every connect.
type PlayerClaims struct {
	GameID string `json:"game_id"`
	UserID string `json:"user_id"`
	Color  string `json:"color"` // "WHITE" or "BLACK"
	jwt.RegisteredClaims
}

// SignPlayerToken creates a signed HS256 JWT from the provided claims.
// The caller must set ExpiresAt in RegisteredClaims before calling (24h recommended).
func SignPlayerToken(claims PlayerClaims, secret string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth.SignPlayerToken gameID=%s userID=%s: %w",
			claims.GameID, claims.UserID, err)
	}
	return signed, nil
}

// VerifyPlayerToken parses and validates a signed player token.
//
// Returns ErrTokenExpired if the token is structurally valid but past its
// expiry time. Returns ErrTokenInvalid for all other failures: wrong secret,
// tampered payload, unsupported signing algorithm, or malformed token string.
//
// The keyFunc enforces HS256 exclusively to prevent algorithm confusion attacks.
// A token presenting alg=none or alg=RS256 is rejected before signature
// verification even begins.
func VerifyPlayerToken(tokenString string, secret string) (*PlayerClaims, error) {
	claims := &PlayerClaims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	if !token.Valid {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}

// --- PHASE_2.md Step 4: ConnectClaims ---------------------------------------

// ConnectClaimsTTL is the fixed lifetime of a ConnectClaims token, per
// DECISIONS_LOG_PHASE_2.md ADR-022: deliberately short (10s) since it only
// needs to survive the gap between a resolve response and the client's
// immediately-following WebSocket dial — not a reconnection window like
// PlayerClaims' 24h. Exported so Step 5's resolve handler has a single
// source of truth rather than hardcoding "10 * time.Second" at the mint site.
const ConnectClaimsTTL = 10 * time.Second

// ConnectClaims are the JWT claims embedded in the short-lived routing
// credential minted by the Step 5 resolve endpoint (GET /games/:id/resolve)
// and verified by the WebSocket upgrade handler at the masked
// /connect/{instanceLabel} URL (Step 8). See DECISIONS_LOG_PHASE_2.md
// ADR-022 for the full resolve-then-connect rationale and the two-token
// split (long-lived PlayerClaims authenticate the resolve call itself;
// ConnectClaims authenticate only the WebSocket upgrade that follows it).
//
// InstanceLabel is opaque outside the Edge Proxy's static label→upstream map
// (ADR-022) — it is never a real host/address, only meaningful to nginx's
// mechanical dereference.
//
// Field naming/shape deliberately mirrors PlayerClaims: Color is string, not
// store.Color, keeping internal/auth free of store dependencies — the game
// layer validates the color value after verification, same as PlayerClaims.
type ConnectClaims struct {
	GameID        string `json:"game_id"`
	UserID        string `json:"user_id"`
	Color         string `json:"color"` // "WHITE" or "BLACK"
	InstanceLabel string `json:"instance_label"`
	jwt.RegisteredClaims
}

// SignConnectToken creates a signed HS256 JWT from the provided claims.
// The caller must set ExpiresAt in RegisteredClaims before calling —
// time.Now().Add(ConnectClaimsTTL) per ADR-022. Signed with the same secret
// as SignPlayerToken (ADR-022: "same signing key as PlayerClaims") — there is
// only ever one JWT signing secret in this codebase; ConnectClaims and
// PlayerClaims are distinguished by their claim shape and by which endpoint
// accepts which, not by using different keys.
func SignConnectToken(claims ConnectClaims, secret string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth.SignConnectToken gameID=%s userID=%s: %w",
			claims.GameID, claims.UserID, err)
	}
	return signed, nil
}

// VerifyConnectToken parses and validates a signed connect token.
//
// Returns ErrTokenExpired if the token is structurally valid but past its
// expiry time — the expected, common case given ConnectClaimsTTL's
// deliberately short 10s window (PHASE_2.md Step 5 requires the WebSocket
// upgrade handler to reject this cleanly, not panic or hang, and to signal
// the client should re-call resolve rather than retry the stale masked URL).
// Returns ErrTokenInvalid for all other failures: wrong secret, tampered
// payload, unsupported signing algorithm, or malformed token string. Reuses
// the same two sentinels as VerifyPlayerToken rather than introducing
// ConnectClaims-specific ones — the failure semantics are identical for both
// token types, and both live in this same package.
//
// Same HS256-only enforcement as VerifyPlayerToken, same reasoning (prevent
// algorithm confusion attacks).
//
// Does NOT check claims.GameID or claims.Color against any expected value —
// consistent with VerifyPlayerToken's existing contract (see
// internal/api/ws_handler.go's ServeHTTP, which checks claims.GameID against
// the URL param itself after calling VerifyPlayerToken). The Step 8 WSHandler
// change performs the same match check for ConnectClaims, for the same
// reason: scope-matching is call-site-specific (what is it being matched
// against?), not a property the auth package can enforce generically.
func VerifyConnectToken(tokenString string, secret string) (*ConnectClaims, error) {
	claims := &ConnectClaims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	if !token.Valid {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}