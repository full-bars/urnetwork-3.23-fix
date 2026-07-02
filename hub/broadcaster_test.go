package main

import (
	"testing"
	"time"
)

func TestBroadcasterPublishDeliversToAllSubscribers(t *testing.T) {
	b := newBroadcaster()
	ch1 := b.subscribe()
	ch2 := b.subscribe()

	b.publish()

	select {
	case <-ch1:
	default:
		t.Errorf("ch1 did not receive a notification")
	}
	select {
	case <-ch2:
	default:
		t.Errorf("ch2 did not receive a notification")
	}
}

func TestBroadcasterPublishNonBlockingWithPendingNotification(t *testing.T) {
	b := newBroadcaster()
	ch := b.subscribe()

	done := make(chan struct{})
	go func() {
		b.publish()
		b.publish() // ch's buffer is already full after the first publish; must not block
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked with a full subscriber buffer")
	}

	select {
	case <-ch:
	default:
		t.Errorf("ch did not receive the coalesced notification")
	}
}

func TestBroadcasterUnsubscribeStopsDelivery(t *testing.T) {
	b := newBroadcaster()
	ch := b.subscribe()
	b.unsubscribe(ch)

	b.publish()

	select {
	case <-ch:
		t.Errorf("unsubscribed channel received a notification")
	default:
	}
}

func TestBroadcasterPublishOnNilReceiverIsSafe(t *testing.T) {
	var b *broadcaster
	b.publish() // must not panic — many existing store tests build &store{} without a broadcaster
}
