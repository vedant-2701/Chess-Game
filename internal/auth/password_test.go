package auth_test

import (
	"testing"

	"github.com/vedant-2701/chess/internal/auth"
)

func TestHashPassword_VerifyPassword_RoundTrip(t *testing.T) {
	hash, err := auth.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("expected a non-empty hash")
	}
	if hash == "correct-horse-battery-staple" {
		t.Fatal("HashPassword returned the plaintext unchanged — not hashed")
	}

	ok, err := auth.VerifyPassword("correct-horse-battery-staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("expected the correct password to verify")
	}
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("right-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := auth.VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword returned an error for a normal mismatch (should return false, nil): %v", err)
	}
	if ok {
		t.Error("expected the wrong password to fail verification")
	}
}

func TestHashPassword_SameInputProducesDifferentHashes(t *testing.T) {
	// bcrypt salts internally — two calls with the same plaintext must never
	// produce identical hashes. A regression here (e.g. someone swapping in
	// a plain unsalted hash function) would silently make every account
	// with the same password share a stored value.
	h1, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword (1): %v", err)
	}
	h2, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword (2): %v", err)
	}
	if h1 == h2 {
		t.Error("two hashes of the same password must differ (bcrypt salts internally)")
	}

	// Both must still verify correctly despite differing.
	for i, h := range []string{h1, h2} {
		ok, err := auth.VerifyPassword("same-password", h)
		if err != nil {
			t.Fatalf("VerifyPassword (%d): %v", i, err)
		}
		if !ok {
			t.Errorf("hash %d did not verify against its own plaintext", i)
		}
	}
}
