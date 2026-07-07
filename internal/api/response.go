package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// dataEnvelope and errorEnvelope are the two JSON response shapes required by
// CODING_GUIDELINES.md §7 for every response in this package — REST and the
// WS pre-upgrade rejection path alike. One envelope implementation, not one
// per handler file.
type dataEnvelope struct {
	Data any `json:"data"`
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// REST-specific error codes not already defined as game.ErrCode* constants
// in internal/game/messages.go (those are scoped to WebSocket ERROR
// messages). game.ErrCodeGameNotFound and game.ErrCodeInternalError are
// reused here directly rather than duplicated, since the same failure modes
// apply over HTTP.
const (
	errCodeInvalidRequest   = "INVALID_REQUEST"
	errCodeGameNotJoinable  = "GAME_NOT_JOINABLE"
	errCodeSelfPlayDisallow = "SELF_PLAY_NOT_ALLOWED"
)

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(dataEnvelope{Data: data}); err != nil {
		slog.Error("writeData: failed to encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{Error: errorDetail{Code: code, Message: message}}); err != nil {
		slog.Error("writeError: failed to encode response", "error", err)
	}
}
