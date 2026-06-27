package game_test

import (
	"context"
	"testing"
	"time"

	"github.com/vedant-2701/chess/internal/game"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestLocalEventBus_PublishWithNoSubscribers(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	// Publish with no subscribers must succeed silently — not an error.
	err := bus.Publish(ctx, game.GameEvent{GameID: "game-1", Type: game.MsgTypeMoveApplied, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatalf("expected nil error publishing with no subscribers, got: %v", err)
	}
}

func TestLocalEventBus_SubscribeReceivesPublishedEvent(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	ch, unsubscribe, err := bus.Subscribe(ctx, "game-1")
	if err != nil {
		t.Fatalf("Subscribe returned unexpected error: %v", err)
	}
	defer unsubscribe()

	want := game.GameEvent{GameID: "game-1", Type: game.MsgTypeMoveApplied, Payload: []byte(`{"san":"e4"}`)}
	if err := bus.Publish(ctx, want); err != nil {
		t.Fatalf("Publish returned unexpected error: %v", err)
	}

	select {
	case got := <-ch:
		if got.GameID != want.GameID || got.Type != want.Type || string(got.Payload) != string(want.Payload) {
			t.Errorf("received event mismatch: got %+v, want %+v", got, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event on subscriber channel")
	}
}

func TestLocalEventBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	ch, unsubscribe, _ := bus.Subscribe(ctx, "game-1")
	unsubscribe()

	// Publish after unsubscribe must not send to the now-closed channel.
	_ = bus.Publish(ctx, game.GameEvent{GameID: "game-1", Type: game.MsgTypeMoveApplied})

	// A closed channel is immediately readable with ok=false.
	// If Publish had sent an event, ok would be true.
	select {
	case evt, ok := <-ch:
		if ok {
			t.Fatalf("received event after unsubscribe: %+v", evt)
		}
		// ok=false: channel closed by unsubscribe, no events delivered — correct.
	default:
		t.Fatal("expected channel to be closed (readable with ok=false), got default")
	}
}

func TestLocalEventBus_MultipleSubscribersSameGame(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	ch1, unsub1, _ := bus.Subscribe(ctx, "game-1")
	ch2, unsub2, _ := bus.Subscribe(ctx, "game-1")
	defer unsub1()
	defer unsub2()

	event := game.GameEvent{GameID: "game-1", Type: game.MsgTypeGameOver, Payload: []byte(`{}`)}
	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish returned unexpected error: %v", err)
	}

	for i, ch := range []<-chan game.GameEvent{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Type != event.Type {
				t.Errorf("subscriber %d: got type %q, want %q", i+1, got.Type, event.Type)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("subscriber %d did not receive the event", i+1)
		}
	}
}

func TestLocalEventBus_SubscriberForDifferentGameReceivesNothing(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	ch, unsubscribe, _ := bus.Subscribe(ctx, "game-2")
	defer unsubscribe()

	// Publish to game-1, subscriber is on game-2 — must not receive.
	_ = bus.Publish(ctx, game.GameEvent{GameID: "game-1", Type: game.MsgTypeMoveApplied})

	select {
	case evt := <-ch:
		t.Fatalf("subscriber for game-2 received event for game-1: %+v", evt)
	case <-time.After(50 * time.Millisecond):
		// Correct: nothing delivered across game boundaries.
	}
}

func TestLocalEventBus_FullChannelDropsEventWithoutPanic(t *testing.T) {
	bus := game.NewLocalEventBus()
	ctx := context.Background()

	ch, unsubscribe, _ := bus.Subscribe(ctx, "game-1")
	defer unsubscribe()

	// Saturate the channel (buffer size 8) without draining.
	event := game.GameEvent{GameID: "game-1", Type: game.MsgTypeMoveApplied, Payload: []byte(`{}`)}
	for i := 0; i < 8; i++ {
		_ = bus.Publish(ctx, event)
	}

	// This publish must not panic and must not block — the event is silently dropped.
	done := make(chan struct{})
	go func() {
		_ = bus.Publish(ctx, event)
		close(done)
	}()

	select {
	case <-done:
		// Correct: returned without blocking.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Publish blocked on a full channel instead of dropping the event")
	}

	// Drain to confirm buffer has exactly 8 (the dropped one is not there).
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			if count != 8 {
				t.Errorf("expected 8 events in buffer, got %d", count)
			}
			return
		}
	}
}