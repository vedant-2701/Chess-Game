package game

import (
	"context"
	"log/slog"
	"sync"
)

// GameEvent is the envelope published to the EventBus after significant game
// actions. Type matches one of the MsgType* constants defined in messages.go.
// Payload is the fully serialised WebSocket JSON message — subscribers forward
// it directly to *ws.Connection.Send without further marshalling.
type GameEvent struct {
	GameID  string
	Type    string
	Payload []byte
}

// EventBus is the interface for game event pub/sub. In Phase 1 it is
// implemented by LocalEventBus (in-process). In Phase 2 it is replaced by
// RedisEventBus (cross-server) without changing any call sites. See ADR-010.
type EventBus interface {
	// Publish broadcasts event to all current subscribers for event.GameID.
	// Returning nil when there are no subscribers is correct — it is not an error.
	Publish(ctx context.Context, event GameEvent) error

	// Subscribe registers interest in events for gameID. It returns a channel
	// on which events will be delivered, and an unsubscribe function that must
	// be called exactly once when the subscriber is done. Calling unsubscribe
	// closes the channel; the subscriber's range loop will exit cleanly.
	Subscribe(ctx context.Context, gameID string) (<-chan GameEvent, func(), error)
}

// compile-time check: LocalEventBus must implement EventBus.
var _ EventBus = (*LocalEventBus)(nil)

// LocalEventBus is an in-process EventBus for Phase 1. All Publish and
// Subscribe calls resolve within the same OS process with no network hops.
// It is replaced by RedisEventBus in Phase 2 (see ADR-010).
type LocalEventBus struct {
	// mu protects: subscribers.
	// Publish holds mu.RLock() for the entire non-blocking send loop so that
	// unsubscribe (which needs mu.Lock()) cannot close a channel while a
	// Publish is mid-send. This is the only synchronisation needed — no
	// per-subscriber mutex, no recover().
	mu          sync.RWMutex
	subscribers map[string]map[chan GameEvent]struct{}
}

// NewLocalEventBus returns a ready-to-use LocalEventBus.
func NewLocalEventBus() *LocalEventBus {
	return &LocalEventBus{
		subscribers: make(map[string]map[chan GameEvent]struct{}),
	}
}

// Publish delivers event to all subscribers registered for event.GameID.
// Each send is non-blocking: if a subscriber's channel is full, the event is
// dropped and a warning is logged. One slow subscriber must never block
// delivery to others or block the move pipeline.
func (b *LocalEventBus) Publish(_ context.Context, event GameEvent) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	subs, ok := b.subscribers[event.GameID]
	if !ok {
		return nil // no subscribers — not an error
	}

	for ch := range subs {
		select {
		case ch <- event:
		default:
			slog.Warn("EventBus: subscriber channel full, dropping event",
				"gameID", event.GameID, "eventType", event.Type)
		}
	}
	return nil
}

// Subscribe registers a new subscriber for gameID and returns a delivery
// channel (buffer size 8) and an unsubscribe function. The caller must call
// unsubscribe exactly once — typically via defer — to release resources.
//
// ctx is accepted for interface compatibility with RedisEventBus (Phase 2),
// where it cancels the Redis subscription. LocalEventBus ignores it.
func (b *LocalEventBus) Subscribe(_ context.Context, gameID string) (<-chan GameEvent, func(), error) {
	ch := make(chan GameEvent, 8)

	b.mu.Lock()
	if b.subscribers[gameID] == nil {
		b.subscribers[gameID] = make(map[chan GameEvent]struct{})
	}
	b.subscribers[gameID][ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if subs, ok := b.subscribers[gameID]; ok {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(b.subscribers, gameID)
			}
		}
		// Closing the channel under the write lock is safe: Publish holds
		// RLock for its entire send loop, so it cannot be mid-send when
		// we reach this close. The subscriber's range loop exits on close.
		close(ch)
	}

	return ch, unsubscribe, nil
}