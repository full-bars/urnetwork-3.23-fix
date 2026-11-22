package connect

// The TLS handshake workers pass a context.CancelFunc as the panic handler of
// every worker they start (outboxLoop, runHandshake, the timeout watcher), so a
// panicking worker is supposed to retire its epoch. A type switch matches the
// handler's exact dynamic type, and context.CancelFunc is a defined type, so the
// func() case never took it and the cancel never ran.

import (
	"context"
	"testing"
	"time"
)

// A panic handler declared as a context.CancelFunc must run when the worker
// panics.
func TestHandleErrorRunsCancelFuncHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	func() {
		defer func() {
			// HandleError recovers, so this must not be reached
			recover()
		}()
		HandleError(
			func() { panic("worker panic") },
			context.CancelFunc(cancel),
		)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context.CancelFunc handler did not run after the worker panicked")
	}
}

// The plain func() handler keeps working, and a func(error) handler still sees
// the panic value as an error.
func TestHandleErrorRunsPlainAndErrorHandlers(t *testing.T) {
	ran := 0
	var seen error
	HandleError(
		func() { panic("worker panic") },
		func() { ran += 1 },
		func(err error) { seen = err },
	)
	if ran != 1 {
		t.Fatalf("func() handler ran %d times, want 1", ran)
	}
	if seen == nil {
		t.Fatal("func(error) handler did not receive the panic value")
	}
}
