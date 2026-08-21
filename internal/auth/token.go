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

// DefaultConnectClaimsTTL is ConnectClaimsTTL's default value
// (PHASE_3.md Step 5) when CONNECT_CLAIMS_TTL_SECONDS is unset, per
// DECISIONS_LOG_PHASE_2.md ADR-022: deliberately short (10s) since it only
// needs to survive the gap between a resolve response and the client's
// immediately-following WebSocket dial — not a reconnection window like
// PlayerClaims' 24h.
//
// **Fixed here (PHASE_3.md Step 5):** this was hardcoded at 60s, with this
// exact doc comment already claiming "10s" — changed for local
// manual-testing convenience at some point and never reverted. Corrected to
// match the value the doc comment (and ADR-022) always claimed.
//
// Not read directly by SignConnectToken's production call site anymore
// (internal/game.Manager holds its own resolved TTL, injected via
// NewManager, per CODING_GUIDELINES.md §5's no-global-state rule — a
// package-level var read directly at the mint site would have been the
// simpler fix but violates that rule). This constant is now only the
// well-known default value cmd/server/main.go falls back to when
// CONNECT_CLAIMS_TTL_SECONDS is unset, and token_test.go's fixture value.
const DefaultConnectClaimsTTL = 30 * time.Second

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
// time.Now().Add(<the resolved ConnectClaimsTTL>) per ADR-022 — see
// internal/game.Manager's connectClaimsTTL field, not this package's
// DefaultConnectClaimsTTL directly (PHASE_3.md Step 5). Signed with the same
// secret as SignPlayerToken (ADR-022: "same signing key as PlayerClaims") —
// there is only ever one JWT signing secret in this codebase; ConnectClaims
// and PlayerClaims are distinguished by their claim shape and by which
// endpoint accepts which, not by using different keys.
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
// expiry time — the expected, common case given ConnectClaims' deliberately
// short window (PHASE_2.md Step 5 requires the WebSocket upgrade handler to
// reject this cleanly, not panic or hang, and to signal the client should
// re-call resolve rather than retry the stale masked URL). Returns ErrTokenInvalid for all other failures: wrong secret, tampered
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

// --- PHASE_3.md Step 4: MatchmakingClaims -----------------------------------

// DefaultMatchmakingClaimsTTL is MatchmakingClaimsTTL's default value
// (DECISIONS_LOG_PHASE_3.md ADR-036 §15's addendum: "MatchmakingClaimsTTL
// env-configurable, default 10s") when MATCHMAKING_CLAIMS_TTL_SECONDS is
// unset. Not read directly by SignMatchmakingToken's production call site
// (internal/mmsvc.Handler holds its own resolved TTL, injected via
// NewHandler — same no-global-state reasoning as
// DefaultConnectClaimsTTL above). Only the well-known default value
// cmd/matchmaking-service/main.go falls back to when unset.
const DefaultMatchmakingClaimsTTL = 60 * time.Second

// MatchmakingClaims are the JWT claims scoping a player's matchmaking queue
// session: POST /matchmaking/queue's response, and the credential presented
// to GET /matchmaking/stream, GET /matchmaking/status, and
// DELETE /matchmaking/queue (DECISIONS_LOG_PHASE_3.md ADR-036). A distinct
// type from PlayerClaims — not a reuse with an empty GameID — because
// PlayerClaims.GameID has no meaningful value at queue-time (the whole
// point of matchmaking is that no game exists yet), and inventing a
// placeholder would corrupt PlayerClaims' meaning for every other consumer.
// Same signing secret as PlayerClaims/ConnectClaims — this codebase's
// existing one-secret/multiple-claim-shapes convention, not a new key.
type MatchmakingClaims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

// SignMatchmakingToken creates a signed HS256 JWT from the provided claims.
// The caller must set ExpiresAt in RegisteredClaims before calling —
// time.Now().Add(<the resolved MatchmakingClaimsTTL>), same pattern as
// SignConnectToken — see internal/mmsvc.Handler's matchmakingClaimsTTL
// field, not this package's DefaultMatchmakingClaimsTTL directly.
func SignMatchmakingToken(claims MatchmakingClaims, secret string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth.SignMatchmakingToken userID=%s: %w", claims.UserID, err)
	}
	return signed, nil
}

// VerifyMatchmakingToken parses and validates a signed matchmaking token.
// Same sentinel-error contract as VerifyPlayerToken/VerifyConnectToken
// (ErrTokenExpired vs. ErrTokenInvalid) and the same HS256-only enforcement
// against algorithm confusion attacks. Not yet called anywhere in this
// commit — the endpoints that verify this token (GET /matchmaking/stream,
// GET /matchmaking/status, DELETE /matchmaking/queue) are later Step 4
// checklist items — but defined alongside Sign for the same reason
// VerifyConnectToken sits next to SignConnectToken: one token type, one
// place its full Sign/Verify contract lives.
func VerifyMatchmakingToken(tokenString string, secret string) (*MatchmakingClaims, error) {
	claims := &MatchmakingClaims{}

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