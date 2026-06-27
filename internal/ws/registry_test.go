package ws

import (
	"sync"
	"testing"
	"time"
)

// newTestConnection builds a Connection with no real network socket.
// wsConn is left nil deliberately — these tests only exercise Registry
// bookkeeping and Connection fields that don't touch the socket
// (outboundQueue, closeSig, lastPongReceived). Any test that needs an
// actual wsConn.WriteMessage/ReadMessage call belongs in the integration
// test file instead.
func newTestConnection(id string) *Connection {
	return &Connection{
		ID:               id,
		outboundQueue:    make(chan outboundMessage, outboundQueueSize),
		closeSig:         make(chan struct{}),
		lastPongReceived: time.Now(),
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	conn := newTestConnection("alice")

	r.Register("alice", conn)

	got, exists := r.Get("alice")
	if !exists {
		t.Fatal("expected alice to exist in registry after Register")
	}
	if got != conn {
		t.Fatal("Get returned a different *Connection than was registered")
	}
}

func TestRegistry_GetMissing(t *testing.T) {
	r := NewRegistry()

	_, exists := r.Get("nobody")
	if exists {
		t.Fatal("expected exists=false for a user_id that was never registered")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	conn := newTestConnection("alice")
	r.Register("alice", conn)

	r.Unregister("alice")

	_, exists := r.Get("alice")
	if exists {
		t.Fatal("expected alice to be gone after Unregister")
	}
}

// TestRegistry_UnregisterMissingIsNoop confirms the explicit design
// decision: unregistering an id that isn't present must not panic or
// error — it can legitimately happen (e.g. a connection already cleaned
// up via another path).
func TestRegistry_UnregisterMissingIsNoop(t *testing.T) {
	r := NewRegistry()

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Unregister on missing id must not panic, got: %v", recovered)
		}
	}()

	r.Unregister("nobody")
}

func TestRegistry_RegisterOverwritesExisting(t *testing.T) {
	r := NewRegistry()
	first := newTestConnection("alice")
	second := newTestConnection("alice")

	r.Register("alice", first)
	r.Register("alice", second)

	got, _ := r.Get("alice")
	if got != second {
		t.Fatal("expected second Register call to overwrite the first connection for the same id")
	}
}

func TestRegistry_Broadcast_DeliversToAllConnections(t *testing.T) {
	r := NewRegistry()
	ids := []string{"alice", "bob", "carol"}
	conns := make(map[string]*Connection)

	for _, id := range ids {
		c := newTestConnection(id)
		conns[id] = c
		r.Register(id, c)
	}

	r.Broadcast([]byte("hello"))

	for _, id := range ids {
		select {
		case msg := <-conns[id].outboundQueue:
			if string(msg.payload) != "hello" {
				t.Fatalf("connection %s got wrong payload: %s", id, msg.payload)
			}
		default:
			t.Fatalf("connection %s did not receive the broadcast message", id)
		}
	}
}

// TestRegistry_Broadcast_OneFullQueueDoesNotBlockOthers verifies that a
// single backpressured connection doesn't prevent delivery to the rest
// of the registry — this is the entire reason Broadcast logs Send errors
// instead of treating them as fatal.
func TestRegistry_Broadcast_OneFullQueueDoesNotBlockOthers(t *testing.T) {
	r := NewRegistry()

	full := newTestConnection("full")
	// Saturate full's queue so the next Send returns ErrQueueFull.
	for i := 0; i < outboundQueueSize; i++ {
		full.outboundQueue <- outboundMessage{messageType: 2, payload: []byte("filler")}
	}

	healthy := newTestConnection("healthy")

	r.Register("full", full)
	r.Register("healthy", healthy)

	r.Broadcast([]byte("hello"))

	select {
	case msg := <-healthy.outboundQueue:
		if string(msg.payload) != "hello" {
			t.Fatalf("healthy connection got wrong payload: %s", msg.payload)
		}
	default:
		t.Fatal("healthy connection should have received the broadcast despite full's queue being full")
	}
}

// TestRegistry_ConcurrentAccess exercises Register/Unregister/Get/Broadcast
// concurrently. Run with -race to catch any data race in the RWMutex usage.
func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a' + n%26))
			conn := newTestConnection(id)
			r.Register(id, conn)
			r.Get(id)
			r.Broadcast([]byte("ping"))
			r.Unregister(id)
		}(i)
	}

	wg.Wait()
}