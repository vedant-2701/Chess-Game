package ws

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMessageSize      = 512 * 1024 // 512KB
	outboundQueueSize   = 32
	pingInterval        = 30 * time.Second
	pongTimeout         = 60 * time.Second
	shutdownGracePeriod = 5 * time.Second
)

// outboundMessage is the envelope that flows through a Connection's
// outboundQueue. It carries enough information for WriteLoop to perform
// the correct kind of write (data frame vs control frame) without any
// other goroutine ever touching the underlying websocket.Conn directly.
type outboundMessage struct {
	messageType int
	payload     []byte
}

// Connection wraps a single upgraded WebSocket connection along with all
// the state needed to manage its lifecycle: a single-writer outbound
// queue, idempotent close handling, heartbeat bookkeeping, and a
// WaitGroup so shutdown logic can know when this connection's goroutines
// have actually finished.
type Connection struct {
	ID     string
	wsConn *websocket.Conn

	outboundQueue chan outboundMessage
	closeSig      chan struct{}
	closeOnce     sync.Once

	pongMu           sync.RWMutex
	lastPongReceived time.Time

	wg sync.WaitGroup
}

// NewConnection constructs a Connection ready to be registered and have
// its goroutines started. lastPongReceived is seeded to "now" because
// completing the WebSocket handshake is itself proof of liveness.
func NewConnection(id string, wsConn *websocket.Conn) *Connection {
	return &Connection{
		ID:               id,
		wsConn:           wsConn,
		outboundQueue:    make(chan outboundMessage, outboundQueueSize),
		closeSig:         make(chan struct{}),
		lastPongReceived: time.Now(),
	}
}

// Close idempotently tears down the connection. Safe to call concurrently
// and safe to call multiple times (e.g. from both the heartbeat monitor
// and ReadLoop's cleanup path) — sync.Once guarantees the underlying
// close(closeSig) and wsConn.Close() only ever run once.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		close(c.closeSig)
		c.wsConn.Close()
	})
}

// Send enqueues an application data message for delivery. It never blocks
// and never force-closes the connection on backpressure — a full queue
// likely means a temporarily slow client, not a dead one, so the caller
// just gets ErrQueueFull back and can decide what to do (e.g. drop and
// log, in the case of Broadcast).
func (c *Connection) Send(payload []byte) error {
	select {
	case <-c.closeSig:
		return ErrConnectionClosed
	case c.outboundQueue <- outboundMessage{messageType: websocket.TextMessage, payload: payload}:
		return nil
	default:
		return ErrQueueFull
	}
}

// SendCloseFrame is used only during shutdown. Unlike Send, it force-closes
// the connection if the queue is full, because during shutdown the end
// state is "closed" either way — there's no healthy-slow-connection case
// worth preserving here.
func (c *Connection) SendCloseFrame(statusCode int, reason string) error {
	payload := websocket.FormatCloseMessage(statusCode, reason)
	select {
	case <-c.closeSig:
		return ErrConnectionClosed
	case c.outboundQueue <- outboundMessage{messageType: websocket.CloseMessage, payload: payload}:
		return nil
	default:
		c.Close()
		return ErrQueueFull
	}
}

// enqueuePing is the heartbeat monitor's only way to cause a ping to be
// written. It goes through the same single-writer path as everything
// else — no goroutine besides WriteLoop ever calls wsConn.WriteMessage.
func (c *Connection) enqueuePing() error {
	select {
	case <-c.closeSig:
		return ErrConnectionClosed
	case c.outboundQueue <- outboundMessage{messageType: websocket.PingMessage, payload: nil}:
		return nil
	default:
		return ErrQueueFull
	}
}

// WriteLoop is the single writer for this connection's underlying socket.
// Every outbound byte — data, ping, or close frame — passes through here.
func (c *Connection) WriteLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.closeSig:
			return
		case msg, ok := <-c.outboundQueue:
			if !ok {
				return
			}
			if err := c.wsConn.WriteMessage(msg.messageType, msg.payload); err != nil {
				c.Close()
				return
			}
		}
	}
}

// ReadLoop owns the connection's only reader. For every application frame
// received, it calls onMessage with the raw payload. When the loop exits
// for any reason (peer close, network error, or server shutdown via
// closeSig), it calls onClose exactly once before returning. onClose is
// responsible for all cleanup outside the ws layer (e.g. game session
// disconnect handling and ws.Registry unregistration).
//
// NOTE: Review the context passed to game.Manager.HandleMessage at Step 11
// (ws.Handler implementation). The HTTP request context is cancelled when
// ServeHTTP returns, which is before ReadLoop exits. Decide there whether
// to use a derived context or context.Background() for the WS session
// lifetime.
func (c *Connection) ReadLoop(onMessage func([]byte), onClose func()) {
	defer c.wg.Done()
	defer func() {
		onClose()
		c.Close()
	}()

	c.wsConn.SetReadLimit(maxMessageSize)
	c.wsConn.SetPongHandler(func(appData string) error {
		c.pongMu.Lock()
		c.lastPongReceived = time.Now()
		c.pongMu.Unlock()
		return nil
	})

	for {
		_, msg, err := c.wsConn.ReadMessage()
		if err != nil {
			return
		}
		onMessage(msg)
	}
}

func (c *Connection) getLastPongReceived() time.Time {
	c.pongMu.RLock()
	defer c.pongMu.RUnlock()
	return c.lastPongReceived
}

// StartHeartbeatMonitor runs for the lifetime of the connection, sending
// periodic pings and closing the connection if no pong has been observed
// within pongTimeout. It is intentionally one goroutine per connection
// (not one global monitor) so that one connection's heartbeat failing
// never affects any other connection's detection.
func (c *Connection) StartHeartbeatMonitor(pingInterval, timeoutThreshold time.Duration) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeSig:
			return
		case <-ticker.C:
			if time.Since(c.getLastPongReceived()) > timeoutThreshold {
				c.Close()
				return
			}
			if err := c.enqueuePing(); err != nil {
				// Not logged on every failure — a single missed ping is
				// not actionable signal; the timeout check above is the
				// real failure detector. Repeated failures would surface
				// as a timeout shortly after anyway.
				_ = err
			}
		}
	}
}

// waitWithTimeout blocks until wg's counter reaches zero or timeout
// elapses, whichever comes first. Returns true if the WaitGroup finished
// cleanly within the timeout.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
