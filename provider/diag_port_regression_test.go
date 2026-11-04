package main

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestDiagSecondRequestDoesNotUnregisterFirst(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.cpuFor = time.Minute
	started := make(chan struct{})
	env.startCPU = func(w io.Writer) error { close(started); _, err := w.Write([]byte("cpu")); return err }
	done := make(chan error, 1)
	go func() { _, err := runDiagTracked(env); done <- err }()
	<-started
	diagActiveMu.Lock()
	first := diagActive
	diagActiveMu.Unlock()
	if first == nil { t.Fatal("first capture is not registered") }
	_, err := runDiagTracked(env)
	diagActiveMu.Lock()
	still := diagActive
	// Restore the first registration on the old code so the test can clean up.
	if still != first { diagActive = first }
	diagActiveMu.Unlock()
	quiesceControlSocket()
	select {
	case firstErr := <-done:
		if !errors.Is(firstErr, errDiagHotSwap) { t.Errorf("first capture: %v", firstErr) }
	case <-time.After(5 * time.Second): t.Fatal("abort did not reach first capture")
	}
	if err == nil { t.Fatal("second capture was not refused") }
	if still != first { t.Fatal("refused request displaced or cleared first capture") }
}

func TestQuiesceClosesSocketBeforeAbortingDiag(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.cpuFor = time.Minute
	started := make(chan struct{})
	env.startCPU = func(w io.Writer) error { close(started); _, err := w.Write([]byte("cpu")); return err }
	done := make(chan error, 1)
	go func() { _, err := runDiagTracked(env); done <- err }()
	<-started
	var pending bool
	setQuiesceHook(func() { select { case <-done: default: pending = true } })
	t.Cleanup(func() { setQuiesceHook(nil) })
	quiesceControlSocket()
	if !pending { t.Fatal("capture was aborted before socket closed") }
	<-done
}

