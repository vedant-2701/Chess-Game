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

// --- ConnectClaims (PHASE_2.md Step 4) --------------------------------------

const testInstanceLabel = "instance-a"

// validConnectClaims returns a ConnectClaims set with auth.ConnectClaimsTTL's
// expiry — ADR-022's actual production shape, not an arbitrary test value.
func validConnectClaims() auth.ConnectClaims {
	return auth.ConnectClaims{
		GameID:        testGameID,
		UserID:        testUserID,
		Color:         testColor,
		InstanceLabel: testInstanceLabel,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(auth.ConnectClaimsTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
}

func TestSignConnectToken(t *testing.T) {
	t.Run("returns non-empty token string", func(t *testing.T) {
		token, err := auth.SignConnectToken(validConnectClaims(), testSecret)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token == "" {
			t.Error("expected non-empty token string")
		}
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Errorf("expected 3 JWT segments, got %d", len(parts))
		}
	})
}

func TestVerifyConnectToken(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T) string
		secret      string
		wantErr     error
		checkClaims func(t *testing.T, c *auth.ConnectClaims)
	}{
		{
			name: "valid token verifies and returns correct claims",
			setup: func(t *testing.T) string {
				t.Helper()
				tok, err := auth.SignConnectToken(validConnectClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return tok
			},
			secret:  testSecret,
			wantErr: nil,
			checkClaims: func(t *testing.T, c *auth.ConnectClaims) {
				t.Helper()
				// This is PHASE_2.md Step 4's "gameID/color match check" test:
				// confirming the fields survive a real sign→verify round trip
				// intact, not tampered or dropped — not an enforcement check
				// inside VerifyConnectToken itself, which (like
				// VerifyPlayerToken) deliberately leaves scope-matching to the
				// caller. See VerifyConnectToken's doc comment.
				if c.GameID != testGameID {
					t.Errorf("GameID: got %q, want %q", c.GameID, testGameID)
				}
				if c.UserID != testUserID {
					t.Errorf("UserID: got %q, want %q", c.UserID, testUserID)
				}
				if c.Color != testColor {
					t.Errorf("Color: got %q, want %q", c.Color, testColor)
				}
				if c.InstanceLabel != testInstanceLabel {
					t.Errorf("InstanceLabel: got %q, want %q", c.InstanceLabel, testInstanceLabel)
				}
			},
		},
		{
			name: "expired token returns ErrTokenExpired",
			setup: func(t *testing.T) string {
				t.Helper()
				claims := auth.ConnectClaims{
					GameID:        testGameID,
					UserID:        testUserID,
					Color:         testColor,
					InstanceLabel: testInstanceLabel,
					RegisteredClaims: jwt.RegisteredClaims{
						// The realistic expiry case for a 10s-TTL token: not an
						// hour stale (that would also catch a PlayerClaims-style
						// bug), just past its short window — e.g. the client took
						// too long between resolve and dialing the WS.
						ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Second)),
						IssuedAt:  jwt.NewNumericDate(time.Now().Add(-auth.ConnectClaimsTTL - time.Second)),
					},
				}
				tok, err := auth.SignConnectToken(claims, testSecret)
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
				tok, err := auth.SignConnectToken(validConnectClaims(), testSecret)
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
				tok, err := auth.SignConnectToken(validConnectClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				return tok[:len(tok)-1] + "X"
			},
			secret:  testSecret,
			wantErr: auth.ErrTokenInvalid,
		},
		{
			name: "tampered payload (instanceLabel swapped) returns ErrTokenInvalid",
			// ConnectClaims-specific tamper case beyond what PlayerClaims' tests
			// cover: confirms a forged InstanceLabel — an attempt to redirect a
			// legitimately-obtained token at a DIFFERENT instance than the one
			// the resolve call actually chose — fails signature verification
			// exactly like any other payload tamper, since InstanceLabel is
			// inside the signed claims, not a separate unsigned field.
			setup: func(t *testing.T) string {
				t.Helper()
				tok, err := auth.SignConnectToken(validConnectClaims(), testSecret)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				parts := strings.Split(tok, ".")
				if len(parts) != 3 {
					t.Fatalf("expected 3 JWT segments, got %d", len(parts))
				}
				// Flip a character in the MIDDLE of the payload segment, not the
				// last one — base64's final character can fall on "don't care"
				// bits when the segment length isn't a multiple of 4, meaning
				// some last-character flips decode to the identical bytes and
				// would make this test a silent no-op. A middle-index flip always
				// changes a real, meaningful bit position.
				payload := parts[1]
				mid := len(payload) / 2
				midChar := payload[mid]
				replacement := byte('A')
				if midChar == 'A' {
					replacement = 'B'
				}
				tamperedPayload := payload[:mid] + string(replacement) + payload[mid+1:]
				return parts[0] + "." + tamperedPayload + "." + parts[2]
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

			claims, err := auth.VerifyConnectToken(tokenStr, tt.secret)

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

// TestConnectClaims_TTLIsShort guards against a regression where someone
// "fixes" a perceived bug by widening ConnectClaimsTTL to something
// PlayerClaims-like (hours) — that would silently defeat ADR-022's whole
// point (a routing credential that's only ever valid for the resolve→dial
// gap, not a general-purpose session token).
func TestConnectClaims_TTLIsShort(t *testing.T) {
	if auth.ConnectClaimsTTL > time.Minute {
		t.Fatalf("ConnectClaimsTTL = %s, expected a short (seconds-scale) routing-credential lifetime, not a session-token-scale one", auth.ConnectClaimsTTL)
	}
}