package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/vedant-2701/chess/internal/auth"
)

const (
	testSecret  = "test-secret-key-for-unit-tests"
	testGameID  = "550e8400-e29b-41d4-a716-446655440000"
	testUserID  = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	testColor   = "WHITE"
)

// validClaims returns a PlayerClaims set with a 24h expiry.
func validClaims() auth.PlayerClaims {
	return auth.PlayerClaims{
		GameID: testGameID,
		UserID: testUserID,
		Color:  testColor,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
}

func TestSignPlayerToken(t *testing.T) {
	t.Run("returns non-empty token string", func(t *testing.T) {
		token, err := auth.SignPlayerToken(validClaims(), testSecret)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token == "" {
			t.Error("expected non-empty token string")
		}
		// A JWT always has exactly three dot-separated segments
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Errorf("expected 3 JWT segments, got %d", len(parts))
		}
	})
}

func TestVerifyPlayerToken(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) string // returns the token string to verify
		secret    string
		wantErr   error   // nil means expect success
		checkClaims func(t *testing.T, c *auth.PlayerClaims)
	}{
		{
			name: "valid token verifies and returns correct claims",
			setup: func(t *testing.T) string {
				t.Helper()
				tok, err := auth.SignPlayerToken(validClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return tok
			},
			secret:  testSecret,
			wantErr: nil,
			checkClaims: func(t *testing.T, c *auth.PlayerClaims) {
				t.Helper()
				if c.GameID != testGameID {
					t.Errorf("GameID: got %q, want %q", c.GameID, testGameID)
				}
				if c.UserID != testUserID {
					t.Errorf("UserID: got %q, want %q", c.UserID, testUserID)
				}
				if c.Color != testColor {
					t.Errorf("Color: got %q, want %q", c.Color, testColor)
				}
			},
		},
		{
			name: "expired token returns ErrTokenExpired",
			setup: func(t *testing.T) string {
				t.Helper()
				claims := auth.PlayerClaims{
					GameID: testGameID,
					UserID: testUserID,
					Color:  testColor,
					RegisteredClaims: jwt.RegisteredClaims{
						ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
						IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
					},
				}
				tok, err := auth.SignPlayerToken(claims, testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return tok
			},
			secret:  testSecret,
			wantErr: auth.ErrTokenExpired,
		},
		{
			name: "wrong secret returns ErrTokenInvalid",
			setup: func(t *testing.T) string {
				t.Helper()
				tok, err := auth.SignPlayerToken(validClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return tok
			},
			secret:  "wrong-secret",
			wantErr: auth.ErrTokenInvalid,
		},
		{
			name: "tampered signature returns ErrTokenInvalid",
			setup: func(t *testing.T) string {
				t.Helper()
				tok, err := auth.SignPlayerToken(validClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				// Corrupt the last character of the signature segment
				return tok[:len(tok)-1] + "X"
			},
			secret:  testSecret,
			wantErr: auth.ErrTokenInvalid,
		},
		{
			name: "malformed token string returns ErrTokenInvalid",
			setup: func(t *testing.T) string {
				return "not.a.jwt"
			},
			secret:  testSecret,
			wantErr: auth.ErrTokenInvalid,
		},
		{
			name: "empty token string returns ErrTokenInvalid",
			setup: func(t *testing.T) string {
				return ""
			},
			secret:  testSecret,
			wantErr: auth.ErrTokenInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenStr := tt.setup(t)

			claims, err := auth.VerifyPlayerToken(tokenStr, tt.secret)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error: got %v, want %v", err, tt.wantErr)
				}
				if claims != nil {
					t.Error("expected nil claims on error, got non-nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if claims == nil {
				t.Fatal("expected non-nil claims, got nil")
			}
			if tt.checkClaims != nil {
				tt.checkClaims(t, claims)
			}
		})
	}
}