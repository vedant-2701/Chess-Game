package mmsvc

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// dataEnvelope/errorEnvelope mirror internal/api/response.go's identical
// shapes exactly (CODING_GUIDELINES.md §7) — duplicated, not imported,
// since internal/api is chess-server's package and matchmaking-service must
// not depend on it (a separate deployable, ADR-032).
type dataEnvelope struct {
	Data any `json:"data"`
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

// errorDetail carries one field beyond code/message — ExistingGame — used
// only by the 409 ALREADY_IN_ACTIVE_GAME response (PHASE_3.md's envelope
// correction note: "its error envelope carries one field beyond
// code/message for the one case that needs it"). omitempty keeps every
// other error response identical in shape to internal/api's.
type errorDetail struct {
	Code         string        `json:"code"`
	Message      string        `json:"message"`
	ExistingGame *existingGame `json:"existingGame,omitempty"`
}

// existingGame mirrors the active-game marker's shape (PHASE_3.md's 409
// response). PlayerToken, not ConnectToken (PHASE_3_DESIGN_NOTES.md §18,
// 2026-08-17) — this token is always a long-lived PlayerClaims, read from
// a marker that may have been written an arbitrary time ago; a client MUST
// call GET /games/{id}/resolve with it before dialing. Contrast with
// matchFoundData (hub.go), which carries a genuinely dialable ConnectToken
// — the two are no longer the same shape, deliberately, since they are no
// longer the same kind of credential.
type existingGame struct {
	GameID        string `json:"gameID"`
	PlayerToken   string `json:"playerToken"`
	InstanceLabel string `json:"instanceLabel"`
	WSPath        string `json:"wsPath"`
}

const (
	errCodeInvalidRequest      = "INVALID_REQUEST"
	errCodeInternal            = "INTERNAL_ERROR"
	errCodeAlreadyInActiveGame = "ALREADY_IN_ACTIVE_GAME"
)

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(dataEnvelope{Data: data}); err != nil {
		slog.Error("mmsvc.writeData: failed to encode response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{Error: errorDetail{Code: code, Message: message}}); err != nil {
		slog.Error("mmsvc.writeError: failed to encode response", "error", err)
	}
}

func writeErrorWithExistingGame(w http.ResponseWriter, status int, code, message string, eg existingGame) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(errorEnvelope{Error: errorDetail{Code: code, Message: message, ExistingGame: &eg}}); err != nil {
		slog.Error("mmsvc.writeErrorWithExistingGame: failed to encode response", "error", err)
	}
}
