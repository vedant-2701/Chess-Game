package mmsvc

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds matchmaking-service's chi router. Mirrors
// internal/api.NewRouter's shape (RequestID/logging/Recoverer middleware,
// same request-logging convention) — a separate router, not a shared one,
// since these are two independent binaries (ADR-032).
func NewRouter(handler *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(requestLoggingMiddleware)
	r.Use(middleware.Recoverer)

	r.Post("/matchmaking/queue", handler.Queue)
	r.Get("/matchmaking/stream", handler.Stream)
	r.Get("/matchmaking/status", handler.Status)
	r.Delete("/matchmaking/queue", handler.Cancel)
	r.Get("/health", handler.Health)

	return r
}

// requestLoggingMiddleware mirrors internal/api's identical middleware —
// CODING_GUIDELINES.md §4 requires log/slog exclusively, which rules out
// chi's built-in middleware.Logger (stdlib log). Duplicated, not imported,
// same reasoning as this package's other duplicated helpers.
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
