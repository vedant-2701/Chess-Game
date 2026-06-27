package ws

import (
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"
)

// Registry is the single source of truth for "which user is connected to
// this server instance". It does pure bookkeeping only — it never closes
// connections itself (that would violate single-responsibility; lifecycle
// decisions belong to the connection's own goroutines and the heartbeat
// monitor, not to the registry).
type Registry struct {
	// registryMu protects: connections
	registryMu  sync.RWMutex
	connections map[string]*Connection
}

func NewRegistry() *Registry {
	return &Registry{
		connections: make(map[string]*Connection),
	}
}

func (r *Registry) Register(id string, conn *Connection) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	r.connections[id] = conn
}

// Unregister is a safe no-op if id is not present — this can legitimately
// happen (e.g. a connection already cleaned itself up via another path).
func (r *Registry) Unregister(id string) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	delete(r.connections, id)
}

func (r *Registry) Get(id string) (*Connection, bool) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()
	conn, exists := r.connections[id]
	return conn, exists
}

// Broadcast fans a message out to every currently registered connection.
// Per-connection send failures are logged, not escalated — one slow or
// closing client should never abort delivery to everyone else.
func (r *Registry) Broadcast(msg []byte) {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()
	for id, conn := range r.connections {
		if err := conn.Send(msg); err != nil {
			slog.Warn("broadcast: failed to send to connection", "connID", id, "error", err)
		}
	}
}

// CloseAll is the registry's contribution to graceful shutdown. It must
// never hold registryMu while triggering a connection's cleanup path,
// since that cleanup calls back into Unregister (same lock) — hence the
// snapshot-then-release pattern below.
func (r *Registry) CloseAll() {
	r.registryMu.RLock()
	conns := make([]*Connection, 0, len(r.connections))
	for _, conn := range r.connections {
		conns = append(conns, conn)
	}
	r.registryMu.RUnlock()

	for _, conn := range conns {
		_ = conn.SendCloseFrame(websocket.CloseNormalClosure, "Server shutting down")
	}

	var shutdownWG sync.WaitGroup
	for _, conn := range conns {
		shutdownWG.Add(1)
		go func(c *Connection) {
			defer shutdownWG.Done()
			if !waitWithTimeout(&c.wg, shutdownGracePeriod) {
				slog.Warn("connection did not close gracefully, forcing", "connID", c.ID)
				c.Close()
			}
		}(conn)
	}
	shutdownWG.Wait()
}
