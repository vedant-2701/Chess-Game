//go:build tools
package tools

import (
	_ "github.com/go-chi/chi/v5"
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/golang-migrate/migrate/v4"
	_ "github.com/gorilla/websocket"
	_ "github.com/notnil/chess"
	_ "go.uber.org/goleak"
)
