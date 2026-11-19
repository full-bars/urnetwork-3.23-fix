package urnettools

import (
	"errors"
	"testing"
	"time"
)

type failingEventWriter struct{}

func (failingEventWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestExtenderEventsTextWriteErrors(t *testing.T) {
	for _, events := range [][]ExtenderGossipEvent{nil, {{Kind: "peer", Time: time.Unix(100, 0)}}} {
		if err := formatExtenderGossipEvents(failingEventWriter{}, events, extenderEventsFilter{}, time.Now()); err == nil {
			t.Fatal("expected write error")
		}
	}
}
