package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// The accept loop treated EVERY Accept error as "the listener is gone" and
// returned. A transient EMFILE during a connection spike, which is a real
// risk on a node carrying thousands of proxies, therefore killed the
// listener permanently: the socket file stays on disk, nothing is
// listening, and the operator is locked out of every live setting until the
// provider restarts.
//
// A closed listener must still end the loop. Only that.
func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantStop bool
	}{
		{"file descriptors exhausted", syscallEMFILE(), false},
		{"listener closed", net.ErrClosed, true},
		{"closed wrapped", &net.OpError{Op: "accept", Err: net.ErrClosed}, true},
	} {
		if got := acceptLoopShouldStop(tc.err); got != tc.wantStop {
			t.Errorf("%s: acceptLoopShouldStop = %v, want %v", tc.name, got, tc.wantStop)
		}
	}
}

func syscallEMFILE() error {
	return &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", errors.New("too many open files"))}
}

// SetReadDeadline does not cover writes. A client that sends a request and
// then stops reading blocks the response write forever, leaking a goroutine
// and a descriptor per connection.
func TestHandlerSetsAWriteDeadline(t *testing.T) {
	b, err := os.ReadFile("control_socket.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "SetWriteDeadline") {
		t.Error("handleControlConn sets no write deadline; a client that stops reading blocks the response forever")
	}
}
