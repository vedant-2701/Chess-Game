//go:build integration

package mmsvc

import (
	"testing"
	"time"
)

func TestHub_Notify_NoRegistrationReturnsFalse(t *testing.T) {
	h := NewHub()
	ok := h.Notify("nobody-connected", EventMatchFound, matchFoundData{GameID: "g"})
	if ok {
		t.Error("expected Notify to return false when no stream is registered")
	}
}

func TestHub_RegisterThenNotify_DeliversToChannel(t *testing.T) {
	h := NewHub()
	ch := h.register("user-a")
	defer h.unregister("user-a", ch)

	data := matchFoundData{GameID: "g-1", ConnectToken: "tok"}
	ok := h.Notify("user-a", EventMatchFound, data)
	if !ok {
		t.Fatal("expected Notify to return true once registered")
	}

	select {
	case evt := <-ch:
		if evt.event != EventMatchFound {
			t.Errorf("event = %q, want %q", evt.event, EventMatchFound)
		}
		got, ok := evt.data.(matchFoundData)
		if !ok {
			t.Fatalf("data has unexpected type %T", evt.data)
		}
		if got != data {
			t.Errorf("data = %+v, want %+v", got, data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the event on the channel")
	}
}

func TestHub_Unregister_PointerIdentityGuard(t *testing.T) {
	// Guards the exact race unregister's doc comment describes: a reconnect
	// (new register()) must not have its channel deleted by a slower
	// deferred unregister() call from the OLD connection.
	h := NewHub()

	oldCh := h.register("user-b")
	newCh := h.register("user-b") // simulates a reconnect before the old cleanup runs

	if oldCh == newCh {
		t.Fatal("test setup invalid: register should return a fresh channel each call")
	}

	h.unregister("user-b", oldCh) // the stale cleanup — must be a no-op against the new registration

	ok := h.Notify("user-b", EventMatchmakingFailed, matchmakingFailedData{Reason: "TEST"})
	if !ok {
		t.Error("expected the NEW registration to still be present after the OLD channel's unregister call")
	}

	select {
	case <-newCh:
		// delivered to the current channel — correct.
	default:
		t.Error("expected the event to have been delivered to the new channel")
	}
}

func TestHub_Notify_FullChannelDropsRatherThanBlocks(t *testing.T) {
	h := NewHub()
	ch := h.register("user-c")
	defer h.unregister("user-c", ch)

	// The channel buffer is 4 (hub.go). Fill it, then confirm a 5th Notify
	// does not block the caller (this test itself would hang otherwise) and
	// reports false.
	for i := 0; i < 4; i++ {
		if !h.Notify("user-c", EventMatchFound, i) {
			t.Fatalf("Notify %d: expected true while the buffer has room", i)
		}
	}
	ok := h.Notify("user-c", EventMatchFound, "overflow")
	if ok {
		t.Error("expected Notify to return false once the buffer is full")
	}
}
