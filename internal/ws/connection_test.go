package ws

import (
	"errors"
	"testing"
	"time"
)

func TestConnection_SendSucceedsWhenQueueHasRoom(t *testing.T) {
	conn := newTestConnection("alice")

	err := conn.Send([]byte("hello"))
	if err != nil {
		t.Fatalf("expected Send to succeed, got: %v", err)
	}

	select {
	case msg := <-conn.outboundQueue:
		if string(msg.payload) != "hello" {
			t.Fatalf("unexpected payload: %s", msg.payload)
		}
	default:
		t.Fatal("expected message to be enqueued")
	}
}

func TestConnection_SendReturnsErrQueueFullWhenSaturated(t *testing.T) {
	conn := newTestConnection("alice")

	for i := 0; i < outboundQueueSize; i++ {
		if err := conn.Send([]byte("filler")); err != nil {
			t.Fatalf("unexpected error filling queue: %v", err)
		}
	}

	err := conn.Send([]byte("one too many"))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got: %v", err)
	}
}

// TestConnection_SendDoesNotCloseOnQueueFull is the regression test for
// the design correction made mid-session: a full queue must signal
// backpressure to the caller, not force-close a possibly-healthy
// connection.
func TestConnection_SendDoesNotCloseOnQueueFull(t *testing.T) {
	conn := newTestConnection("alice")

	for i := 0; i < outboundQueueSize; i++ {
		_ = conn.Send([]byte("filler"))
	}
	_ = conn.Send([]byte("one too many"))

	select {
	case <-conn.closeSig:
		t.Fatal("Send must not close the connection on backpressure (ErrQueueFull)")
	default:
		// closeSig still open — correct.
	}
}

func TestConnection_SendAfterCloseReturnsErrConnectionClosed(t *testing.T) {
	conn := newTestConnection("alice")
	close(conn.closeSig) // simulate closed state without touching wsConn

	err := conn.Send([]byte("hello"))
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("expected ErrConnectionClosed, got: %v", err)
	}
}

// Close() idempotency and concurrent-safety are tested against a real
// *websocket.Conn in integration_test.go (TestConnection_CloseIsIdempotent
// and TestConnection_CloseIsSafeUnderConcurrentCallers), since Close()
// unconditionally calls wsConn.Close() and newTestConnection here
// deliberately constructs a Connection without a real socket.

func TestConnection_EnqueuePingGoesThroughSameQueueAsData(t *testing.T) {
	conn := newTestConnection("alice")

	if err := conn.enqueuePing(); err != nil {
		t.Fatalf("expected enqueuePing to succeed, got: %v", err)
	}

	select {
	case msg := <-conn.outboundQueue:
		if msg.payload != nil {
			t.Fatalf("expected nil payload for a ping, got: %v", msg.payload)
		}
	default:
		t.Fatal("expected ping to be enqueued onto outboundQueue")
	}
}

func TestConnection_LastPongReceivedIsReadableUnderLock(t *testing.T) {
	conn := newTestConnection("alice")
	before := conn.getLastPongReceived()

	time.Sleep(10 * time.Millisecond)
	conn.pongMu.Lock()
	conn.lastPongReceived = time.Now()
	conn.pongMu.Unlock()

	after := conn.getLastPongReceived()
	if !after.After(before) {
		t.Fatal("expected lastPongReceived to advance after update")
	}
}