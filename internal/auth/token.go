package auth

import (
	"errors"
	"fmt"

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