package subscribe_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/subscribe"
)

// drain reads all currently queued updates from the client, returning once no
// update arrives within the given timeout.
func drain(t *testing.T, c *subscribe.Client,
	timeout time.Duration) []interface{} {

	t.Helper()

	var updates []interface{}
	for {
		select {
		case upd := <-c.Updates():
			updates = append(updates, upd)
		case <-time.After(timeout):
			return updates
		}
	}
}

// TestSubscribeFiltered makes sure that filtered clients only receive updates
// that match their predicate, that unmatched updates never enter their
// queue, that matched updates retain arrival order, and that filtering one
// client does not affect other (un)filtered clients.
func TestSubscribeFiltered(t *testing.T) {
	t.Parallel()

	server := subscribe.NewServer()
	if err := server.Start(); err != nil {
		t.Fatalf("unable to start server")
	}
	t.Cleanup(func() {
		_ = server.Stop()
	})

	// One unfiltered client, and one filtered client that only accepts
	// even integers.
	all, err := server.Subscribe()
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}

	evens, err := server.SubscribeFiltered(func(upd interface{}) bool {
		i, ok := upd.(int)
		return ok && i%2 == 0
	})
	if err != nil {
		t.Fatalf("unable to subscribe filtered: %v", err)
	}
	t.Cleanup(all.Cancel)
	t.Cleanup(evens.Cancel)

	const numUpdates = 10
	for i := 0; i < numUpdates; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update: %v", err)
		}
	}

	// The unfiltered client receives everything in order.
	allUpdates := drain(t, all, 200*time.Millisecond)
	if len(allUpdates) != numUpdates {
		t.Fatalf("expected %d updates, got %d", numUpdates,
			len(allUpdates))
	}
	for i, upd := range allUpdates {
		if upd.(int) != i {
			t.Fatalf("unexpected update %v at index %d", upd, i)
		}
	}

	// The filtered client receives the five even values in arrival order.
	evenUpdates := drain(t, evens, 200*time.Millisecond)
	if len(evenUpdates) != numUpdates/2 {
		t.Fatalf("expected %d updates, got %d", numUpdates/2,
			len(evenUpdates))
	}
	for i, upd := range evenUpdates {
		want := i * 2
		if upd.(int) != want {
			t.Fatalf("expected %d, got %v at index %d", want, upd, i)
		}
	}

	// Updates sent after the filtered client canceled must not reach it,
	// even when they match the filter.
	evens.Cancel()
	select {
	case <-evens.Quit():
	case <-time.After(time.Second):
		t.Fatalf("filtered client did not quit")
	}

	for i := numUpdates; i < numUpdates+5; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update: %v", err)
		}
	}

	// The still-active unfiltered client receives the new updates,
	// proving the canceled filtered client did not disturb delivery.
	late := drain(t, all, 200*time.Millisecond)
	if len(late) != 5 {
		t.Fatalf("expected 5 late updates, got %d", len(late))
	}
}

// TestSubscribeFilteredIsolation makes sure that multiple filtered clients
// with different, overlapping predicates only receive their own matching
// updates, each update at most once and in arrival order.
func TestSubscribeFilteredIsolation(t *testing.T) {
	t.Parallel()

	server := subscribe.NewServer()
	if err := server.Start(); err != nil {
		t.Fatalf("unable to start server")
	}
	t.Cleanup(func() {
		_ = server.Stop()
	})

	// Client A accepts even values, client B accepts values >= 4.
	clientA, err := server.SubscribeFiltered(func(upd interface{}) bool {
		i, ok := upd.(int)
		return ok && i%2 == 0
	})
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}
	clientB, err := server.SubscribeFiltered(func(upd interface{}) bool {
		i, ok := upd.(int)
		return ok && i >= 4
	})
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}
	t.Cleanup(clientA.Cancel)
	t.Cleanup(clientB.Cancel)

	for i := 0; i < 6; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update: %v", err)
		}
	}

	gotA := drain(t, clientA, 200*time.Millisecond)
	gotB := drain(t, clientB, 200*time.Millisecond)

	// A: 0,2,4; B: 4,5.
	wantA := []int{0, 2, 4}
	wantB := []int{4, 5}
	if len(gotA) != len(wantA) {
		t.Fatalf("client A expected %d updates, got %d", len(wantA),
			len(gotA))
	}
	if len(gotB) != len(wantB) {
		t.Fatalf("client B expected %d updates, got %d", len(wantB),
			len(gotB))
	}
	for i, want := range wantA {
		if gotA[i].(int) != want {
			t.Fatalf("client A: expected %d, got %v", want, gotA[i])
		}
	}
	for i, want := range wantB {
		if gotB[i].(int) != want {
			t.Fatalf("client B: expected %d, got %v", want, gotB[i])
		}
	}
}

// TestSubscribeFilteredNoneMatches verifies that a predicate rejecting
// everything leaves the client's queue empty while updates keep flowing to
// other clients.
func TestSubscribeFilteredNoneMatches(t *testing.T) {
	t.Parallel()

	server := subscribe.NewServer()
	if err := server.Start(); err != nil {
		t.Fatalf("unable to start server")
	}
	t.Cleanup(func() {
		_ = server.Stop()
	})

	none, err := server.SubscribeFiltered(func(interface{}) bool {
		return false
	})
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}
	some, err := server.SubscribeFiltered(func(upd interface{}) bool {
		_, ok := upd.(string)
		return ok
	})
	if err != nil {
		t.Fatalf("unable to subscribe: %v", err)
	}
	t.Cleanup(none.Cancel)
	t.Cleanup(some.Cancel)

	if err := server.SendUpdate("hello"); err != nil {
		t.Fatalf("unable to send update: %v", err)
	}
	if err := server.SendUpdate(42); err != nil {
		t.Fatalf("unable to send update: %v", err)
	}

	gotNone := drain(t, none, 200*time.Millisecond)
	if len(gotNone) != 0 {
		t.Fatalf("expected no updates, got %d", len(gotNone))
	}

	gotSome := drain(t, some, 200*time.Millisecond)
	if len(gotSome) != 1 || gotSome[0] != "hello" {
		t.Fatalf("expected only the string update, got %v", gotSome)
	}
}
