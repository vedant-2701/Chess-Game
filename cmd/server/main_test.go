package main

import (
	"log/slog"
	"testing"
)

func TestLoadConfig_MissingDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("SERVER_PORT", "")
	t.Setenv("LOG_LEVEL", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for missing DATABASE_URL, got nil")
	}
}

func TestLoadConfig_MissingJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("SERVER_PORT", "")
	t.Setenv("LOG_LEVEL", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for missing JWT_SECRET, got nil")
	}
}

func TestLoadConfig_DefaultsAppliedForOptionalVars(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("SERVER_PORT", "")
	t.Setenv("LOG_LEVEL", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ServerPort != "8080" {
		t.Errorf("ServerPort default: got %q, want %q", cfg.ServerPort, "8080")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %q, want %q", cfg.LogLevel, "info")
	}
}

func TestLoadConfig_ExplicitValuesPassThrough(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.DatabaseURL != "postgres://localhost/test" {
		t.Errorf("DatabaseURL: got %q, want %q", cfg.DatabaseURL, "postgres://localhost/test")
	}
	if cfg.JWTSecret != "test-secret" {
		t.Errorf("JWTSecret: got %q, want %q", cfg.JWTSecret, "test-secret")
	}
	if cfg.ServerPort != "9090" {
		t.Errorf("ServerPort: got %q, want %q", cfg.ServerPort, "9090")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want %q", cfg.LogLevel, "debug")
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  slog.Level
	}{
		{"debug lowercase", "debug", slog.LevelDebug},
		{"debug uppercase", "DEBUG", slog.LevelDebug},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"info explicit", "info", slog.LevelInfo},
		{"empty falls back to info", "", slog.LevelInfo},
		{"unrecognized falls back to info", "garbage", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseLogLevel(tt.input); got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
