package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/vedant-2701/chess/internal/game"
	"github.com/vedant-2701/chess/internal/store"
	"github.com/vedant-2701/chess/internal/ws"
)

// NewRouter builds the complete chi router for the server: REST endpoints
// (game_handler.go) and the WebSocket upgrade endpoint (ws_handler.go)
// mounted side by side, exactly as ARCHITECTURE.md's corrected "internal/api"
// section describes.
//
// wsCtx must be the server-lifetime context required by ADR-018 — see
// NewWSHandler's doc comment. It is threaded through here rather than
// created internally so cmd/server/main.go (Step 13) owns the single
// context.WithCancel call and its matching cancel func for the whole
// process, instead of routes.go silently creating its own.
func NewRouter(manager *game.Manager, userStore *store.UserStore, wsRegistry *ws.Registry, jwtSecret string, wsCtx context.Context) http.Handler {
	gameHandler := NewGameHandler(manager, userStore)
	wsHandler := NewWSHandler(wsCtx, manager, wsRegistry, jwtSecret)

	r := chi.NewRouter()

	// slog request logging. chi's own middleware.Logger uses the stdlib log
	// package by default; CODING_GUIDELINES.md §4 requires log/slog
	// exclusively, so this wraps chi's RequestID/timing conventions with an
	// explicit slog.Info call instead of using middleware.Logger directly.
	r.Use(middleware.RequestID)
	r.Use(requestLoggingMiddleware)
	r.Use(middleware.Recoverer)

	r.Post("/games", gameHandler.CreateGame)
	r.Post("/games/{id}/join", gameHandler.JoinGame)
	r.Get("/games/{id}", gameHandler.GetGame)
	r.Get("/health", gameHandler.Health)
	r.Get("/ws/game/{id}", wsHandler.ServeHTTP)

	return r
}

// requestLoggingMiddleware logs each request via slog, per
// CODING_GUIDELINES.md §4 (log/slog exclusively — no fmt.Println/log.Printf,
// which rules out chi's built-in middleware.Logger).
func requestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		slog.Info("request",
			"requestID", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"durationMs", time.Since(start).Milliseconds(),
		)
	})
}
