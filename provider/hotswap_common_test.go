package main

import (
	"bufio"
	"bytes"
	"testing"
)

func TestHotSwapMessageFraming(t *testing.T) {
	var buf bytes.Buffer

	want := HotswapMessage{
		Type:    HotswapMsgReady,
		Version: "v3.23.0-fix.31.0",
		PID:     12345,
	}

	if err := writeHotswapMessage(&buf, want); err != nil {
		t.Fatalf("writeHotswapMessage: %v", err)
	}

	reader := bufio.NewReader(&buf)
	got, err := readHotswapMessage(reader)
	if err != nil {
		t.Fatalf("readHotswapMessage: %v", err)
	}

	if got.Type != want.Type {
		t.Errorf("got.Type = %v, want %v", got.Type, want.Type)
	}
	if got.Version != want.Version {
		t.Errorf("got.Version = %v, want %v", got.Version, want.Version)
	}
	if got.PID != want.PID {
		t.Errorf("got.PID = %v, want %v", got.PID, want.PID)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("expected non-zero Timestamp in received message")
	}
}
