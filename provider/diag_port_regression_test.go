package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if first == nil {
		t.Fatal("first capture is not registered")
	}
	_, err := runDiagTracked(env)
	diagActiveMu.Lock()
	still := diagActive
	// Restore the first registration on the old code so the test can clean up.
	if still != first {
		diagActive = first
	}
	diagActiveMu.Unlock()
	quiesceControlSocket()
	select {
	case firstErr := <-done:
		if !errors.Is(firstErr, errDiagHotSwap) {
			t.Errorf("first capture: %v", firstErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not reach first capture")
	}
	if err == nil {
		t.Fatal("second capture was not refused")
	}
	if still != first {
		t.Fatal("refused request displaced or cleared first capture")
	}
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
	setQuiesceHook(func() {
		select {
		case <-done:
		default:
			pending = true
		}
	})
	t.Cleanup(func() { setQuiesceHook(nil) })
	quiesceControlSocket()
	if !pending {
		t.Fatal("capture was aborted before socket closed")
	}
	<-done
}

func TestDiagFloorNoLongerScalesWithHeap(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.stats = func() diagStats { return diagStats{HeapInuse: 4600 << 20} }
	env.freeBytes = func(string) (uint64, error) { return 600 << 20, nil }
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatalf("600 MiB free should pass: %v", err)
	}
	env.freeBytes = func(string) (uint64, error) { return 100 << 20, nil }
	_, err := runDiag(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "need 512.0 MiB") {
		t.Fatalf("err = %v, want fixed floor refusal", err)
	}
}

func TestDiagGraceTimeoutDropsCPU(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.cpuStopGrace = 50 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	env.stopCPU = func() { <-stuck }
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "cpu.pb.gz")); !os.IsNotExist(err) {
		t.Fatalf("unfinalized CPU profile committed: %v", err)
	}
	for _, f := range res.Files {
		if f == "cpu.pb.gz" {
			t.Fatalf("Files lists cpu.pb.gz: %v", res.Files)
		}
	}
	summary, err := os.ReadFile(filepath.Join(res.Dir, "summary.txt"))
	if err != nil || !strings.Contains(string(summary), "NOT CAPTURED") {
		t.Fatalf("summary lacks NOT CAPTURED: %v\n%s", err, summary)
	}
}
