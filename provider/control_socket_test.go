package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlSocket_SetGetClear_EndToEnd(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()

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
	resetGlobalControlStateForTest()
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

// TestControlSocket_ConcurrentSetsMemoryMatchesDisk hammers the same key with
// many concurrent `set`s through handleControlRequest and asserts the contract
// that motivated txMu: once every set has settled, the in-memory state and the
// persisted provider_state.json must agree (last-writer-wins with no lost
// update). This is exactly the write that raced before txMu serialized the
// get-old -> set -> persist -> rollback unit — a concurrent set could interleave
// between a persist's snapshot and a rollback, leaving memory and disk
// disagreeing. Run with -race.
//
// Every writer writes a DISTINCT value (w%d-%d, writer index and round), not
// the same literal for all of them — with every writer writing "w", memory
// and disk would trivially agree even if the ordering regression this test
// exists to catch had occurred (any interleaving still lands on "w"). This
// does NOT assert which value ends up final: concurrent completion order is
// unspecified, so the only two things pinned are (1) memory and the
// just-reloaded-from-disk value agree, and (2) whatever that value is, it
// really was one of the values a writer wrote (not corrupted by a lost
// update or a torn persist).
func TestControlSocket_ConcurrentSetsMemoryMatchesDisk(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	const writers = 12
	const rounds = 40
	written := make(map[string]bool, writers*rounds)
	var writtenMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				value := fmt.Sprintf("w%d-%d", w, i)
				writtenMu.Lock()
				written[value] = true
				writtenMu.Unlock()
				handleControlRequest(globalControlState, controlRequest{
					Cmd: "set", Key: "node_name", Value: value,
				})
			}
		}(w)
	}
	wg.Wait()

	// In-memory final value.
	memVal, memFound := globalControlState.get("node_name")
	if !memFound {
		t.Fatalf("node_name should be set after concurrent sets")
	}

	// Reload exactly what the last persist wrote.
	reloaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	diskVal, diskFound := reloaded.get("node_name")
	if !diskFound {
		t.Fatalf("node_name should be persisted after concurrent sets")
	}
	if memVal != diskVal {
		t.Fatalf("memory = %q, disk = %q — concurrent set/persist lost an update", memVal, diskVal)
	}
	if !written[memVal] {
		t.Fatalf("final value %q was never written by any writer — corrupted by the concurrent set/persist path", memVal)
	}
}

func TestIsTruthyOn(t *testing.T) {
	on := []string{"on", "1", "true", "yes"}
	off := []string{"off", "0", "false", "no", "", "2", "bogus"}
	for _, v := range on {
		if !isTruthyOn(v) {
			t.Errorf("isTruthyOn(%q) = false, want true", v)
		}
	}
	for _, v := range off {
		if isTruthyOn(v) {
			t.Errorf("isTruthyOn(%q) = true, want false", v)
		}
	}
}

func TestHotRestartEnabled_GuessBooleanForms(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	cases := []struct {
		stored string
		want   bool
	}{
		{"off", false}, {"0", false}, {"false", false}, {"no", false},
		{"on", true}, {"1", true}, {"true", true}, {"yes", true},
		{"bogus", true}, // unknown value keeps default-on baseline
	}
	for _, tc := range cases {
		globalControlState.set("hot_restart", tc.stored)
		if got := hotRestartEnabled(); got != tc.want {
			t.Errorf("hot_restart=%q -> hotRestartEnabled()=%v, want %v", tc.stored, got, tc.want)
		}
	}
	globalControlState.clear("hot_restart")
	if got := hotRestartEnabled(); got != true {
		t.Errorf("cleared hot_restart (env unset) -> hotRestartEnabled()=%v, want true", got)
	}
}

// TestControlSocket_WaitForReleaseUnblocksWhenListenerGone pins the
// hotswap-takeover gate (main.go candidateAckOnce.Do): a promoted candidate
// must not reload+bind its control socket until the parent's listener at the
// same path is actually gone. While a listener is up, waitForControlSocketRelease
// must block; once it's closed (parent released the socket), it must return
// promptly.
func TestControlSocket_WaitForReleaseUnblocksWhenListenerGone(t *testing.T) {
	withTempHome(t)
	// controlSocketPath() is derived from the HOME redirected by withTempHome.
	path, err := controlSocketPath()
	if err != nil {
		t.Fatalf("controlSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	// 1. Listening: the wait must NOT return within 300ms.
	done := make(chan struct{})
	go func() {
		waitForControlSocketRelease(5 * time.Second)
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("waitForControlSocketRelease returned while the listener was still live")
	case <-time.After(300 * time.Millisecond):
		// correct: blocks while parent is still listening
	}

	// 2. Close the listener (parent released the socket): wait must return.
	ln.Close()
	select {
	case <-done:
		// correct: unblocked promptly after the socket was released
	case <-time.After(2 * time.Second):
		t.Fatalf("waitForControlSocketRelease did not return after the listener closed")
	}
}

// captureTlog runs fn with os.Stdout redirected and returns what tlog wrote.
// tlog goes through fmt.Printf to os.Stdout, so this is the same approach the
// rest of the package's log assertions use.
func captureTlog(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestControlSocketLogsSetAndClear covers the operator-visible half of the
// control socket: a setting that reaches the running provider must say so in
// the provider's own log. Without it, `urnet-tools set` succeeding is visible
// only in the CLI, and there is no way to confirm from the node's logs that
// the daemon actually registered the change.
func TestControlSocketLogsSetAndClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"}); !resp.OK {
			t.Fatalf("set failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "[control] set node_name=nyc-1") {
		t.Errorf("first set must be logged, got: %q", out)
	}
	if !strings.Contains(out, "was unset") {
		t.Errorf("a key with no previous value must log 'was unset', got: %q", out)
	}

	// Overwriting must report the value it replaced, so a log reader can see
	// what the node was running before.
	out = captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-2"}); !resp.OK {
			t.Fatalf("second set failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "set node_name=nyc-2") || !strings.Contains(out, "was nyc-1") {
		t.Errorf("overwrite must log the previous value, got: %q", out)
	}

	out = captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "clear", Key: "node_name"}); !resp.OK {
			t.Fatalf("clear failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "cleared node_name") || !strings.Contains(out, "was nyc-2") {
		t.Errorf("clear must be logged with the previous value, got: %q", out)
	}
}

// TestControlSocketLogsRejectedSet pins that a refused change is logged too:
// a silent rejection is exactly the case where an operator most needs the
// node's log to explain why their setting did not take.
func TestControlSocketLogsRejectedSet(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "not_a_real_key", Value: "x"}); resp.OK {
			t.Fatal("an unknown control key must not be accepted")
		}
	})
	if !strings.Contains(out, "not_a_real_key") || !strings.Contains(out, "rejected") {
		t.Errorf("a rejected set must be logged, got: %q", out)
	}
}

// TestControlSocketGetIsNotLogged keeps reads quiet. `urnet-tools status` polls
// get on every invocation, so logging it would bury the writes that matter.
func TestControlSocketGetIsNotLogged(t *testing.T) {
	withTempHome(t)
	s := newControlState()
	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatal(err)
	}

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "get", Key: "node_name"}); !resp.OK {
			t.Fatalf("get failed: %s", resp.Error)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("get must not log, got: %q", out)
	}
}

// TestDashboardLabelLoggedOnceAndOnChange covers the other half of the
// operator's confirmation path. `urnet-tools rename` and `urnet-tools show-ip`
// take effect at the next renewal rather than immediately, so the node's log is
// the only place their effect can be observed. providerDescription runs per
// proxy per renewal, so the line must appear on first resolve and on change,
// and stay quiet otherwise.
func TestDashboardLabelLoggedOnceAndOnChange(t *testing.T) {
	lastDashboardLabelMu.Lock()
	lastDashboardLabel = ""
	lastDashboardLabelMu.Unlock()

	first := captureTlog(t, func() { logDashboardLabel("nyc-1 [v1]") })
	if !strings.Contains(first, "dashboard label: nyc-1 [v1]") {
		t.Errorf("first resolve must log the label, got: %q", first)
	}

	repeat := captureTlog(t, func() {
		logDashboardLabel("nyc-1 [v1]")
		logDashboardLabel("nyc-1 [v1]")
	})
	if strings.TrimSpace(repeat) != "" {
		t.Errorf("an unchanged label must stay quiet, got: %q", repeat)
	}

	changed := captureTlog(t, func() { logDashboardLabel("nyc-2 [v1]") })
	if !strings.Contains(changed, "nyc-1 [v1] -> nyc-2 [v1]") {
		t.Errorf("a changed label must log the transition, got: %q", changed)
	}
}
