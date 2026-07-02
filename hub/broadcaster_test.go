package main

import (
	"net/http"
	"net/http/httptest"
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

func TestHandleEventsDeliversRefreshOnPublish(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleEvents(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	result := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		result <- string(buf[:n])
	}()

	time.Sleep(50 * time.Millisecond) // let the handler subscribe before publishing
	s.broadcast.publish()

	select {
	case got := <-result:
		if got != "data: refresh\n\n" {
			t.Errorf("body = %q, want %q", got, "data: refresh\n\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestHandleEventsUnsubscribesOnClientDisconnect(t *testing.T) {
	s := &store{
		Nodes:     make(map[string]*nodeState),
		rates:     make(map[string]*nodeRate),
		broadcast: newBroadcaster(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleEvents(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close() // simulate the browser navigating away

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.broadcast.mu.Lock()
		n := len(s.broadcast.subs)
		s.broadcast.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("handler did not unsubscribe after client disconnect")
}
