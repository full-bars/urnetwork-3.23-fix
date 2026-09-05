package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestControlSocket_SetGetClear_EndToEnd(t *testing.T) {
	withTempHome(t)
	globalControlState = newControlState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set response: %+v", resp)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial get: %v", err)
	}
	if !resp.OK || !resp.Found || resp.Value != "nyc-1" {
		t.Fatalf("get response: %+v", resp)
	}

	// The live provider read path sees it immediately — no restart, no poll.
	if got := resolveNodeName("startup-host"); got != "nyc-1" {
		t.Fatalf("resolveNodeName after socket set = %q, want %q", got, "nyc-1")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "clear", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial clear: %v", err)
	}
	if !resp.OK {
		t.Fatalf("clear response: %+v", resp)
	}
	if got := resolveNodeName("startup-host"); got != "startup-host" {
		t.Fatalf("resolveNodeName after clear = %q, want startup default", got)
	}
}

func TestControlSocket_UnknownKeyRejected(t *testing.T) {
	withTempHome(t)
	globalControlState = newControlState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "not-a-real-key", Value: "x"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected rejection for unknown key, got %+v", resp)
	}
}

func TestControlSocket_UnknownCommandRejected(t *testing.T) {
	withTempHome(t)
	globalControlState = newControlState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "delete-everything", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected rejection for unknown command, got %+v", resp)
	}
}

// TestDialControlSocket_NoProviderRunning is the fallback signal PR 3
// (urnet-tools client) depends on: when nothing is listening, the caller
// must be able to tell "provider is down, fall back to the pending-queue
// file" apart from a real protocol error.
func TestDialControlSocket_NoProviderRunning(t *testing.T) {
	withTempHome(t)

	_, err := dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != errNoProvider {
		t.Fatalf("got err=%v, want errNoProvider", err)
	}
}

// TestStartControlSocket_RemovesStaleSocketFile covers the crash-recovery
// path: a previous process left the socket file behind without cleaning up
// (e.g. SIGKILL). A fresh start must reclaim it instead of failing forever.
func TestStartControlSocket_RemovesStaleSocketFile(t *testing.T) {
	home := withTempHome(t)
	globalControlState = newControlState()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stalePath := filepath.Join(dir, "provider.sock")
	if err := os.WriteFile(stalePath, nil, 0o600); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket should reclaim a stale socket file, got: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial after reclaiming stale socket: %v", err)
	}
	if !resp.OK {
		t.Fatalf("get response: %+v", resp)
	}
}

// TestStartControlSocket_RefusesWhenAlreadyListening ensures a second
// startControlSocket call against a socket a live listener already owns
// fails loudly instead of silently stealing it.
func TestStartControlSocket_RefusesWhenAlreadyListening(t *testing.T) {
	withTempHome(t)
	globalControlState = newControlState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	_, err = startControlSocket(ctx, newControlState())
	if err == nil {
		t.Fatalf("expected error starting a second listener on the same socket")
	}
}

func TestStartControlSocket_SocketFilePermissions(t *testing.T) {
	home := withTempHome(t)
	globalControlState = newControlState()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	path := filepath.Join(home, ".urnetwork", "provider.sock")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perms = %o, want 0600 (owner-only)", perm)
	}
}

// TestControlSocket_PersistFailureRollsBackInMemoryState covers the set/
// clear rollback path: if persisting to disk fails after the in-memory
// change was applied, memory and disk must not be left disagreeing about
// what's set.
func TestControlSocket_PersistFailureRollsBackInMemoryState(t *testing.T) {
	home := withTempHome(t)
	globalControlState = newControlState()
	globalControlState.set("node_name", "old-value")

	// Make the state directory read-only so persist() (which needs to
	// create a temp file there) fails, without touching the socket itself.
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // let t.TempDir() clean up

	resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "node_name", Value: "new-value"})
	if resp.OK {
		t.Fatalf("expected persist failure to surface as an error, got %+v", resp)
	}
	if v, _ := globalControlState.get("node_name"); v != "old-value" {
		t.Fatalf("in-memory state after failed persist = %q, want rollback to %q", v, "old-value")
	}
}
