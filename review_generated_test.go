package connect

// Supplementary coverage written during review of PR #350
// (EncryptionMode tri-state + fail-closed gates, scope 2). These target two
// gaps found in the PR's own new tests:
//
//   - TestNotifyRequiredSendBlockedDedup / TestNotifyRequiredReceiveDiscardedDedup
//     (transfer_encrypt_required_test.go) only assert the final state of the
//     `requiredSendBlockedNotified` / `requiredReceiveDiscardedNotified` flag,
//     which is set to true unconditionally on every call in the real
//     implementation regardless of whether the early-return guard fires.
//     Verified by mutation: deleting the guard's early return left those
//     tests green. `TestNotifyRequiredSendBlockedFiresOnlyOnce` below instead
//     observes the actual log side effect (glog is redirected to a pipe for
//     the duration of the test), so a regression that turns the guard into a
//     no-op fails this test.
//   - No coverage existed for the entry gate observing an already-cancelled
//     `sendPack.Ctx` while parked with no cipher under
//     EncryptionModeRequired. TestRequiredGateAlreadyCancelledCtxReturnsPromptly
//     pins that this returns promptly rather than waiting a full poll cycle.

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/glog"
)

// captureStderr redirects the process's stderr (where glog writes, per
// initGlog in connect_test.go) to a pipe for the duration of fn, flushes
// glog, and returns everything written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	fn()

	glog.Flush()
	w.Close()
	os.Stderr = orig

	var sb strings.Builder
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestNotifyRequiredSendBlockedFiresOnlyOnce verifies the log side effect,
// not just the residual flag: calling NotifyRequiredSendBlocked with three
// distinct, greppable reasons must produce exactly one log line, for the
// first reason only.
func TestNotifyRequiredSendBlockedFiresOnlyOnce(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	out := captureStderr(t, func() {
		sess.NotifyRequiredSendBlocked("review-marker-first")
		sess.NotifyRequiredSendBlocked("review-marker-second")
		sess.NotifyRequiredSendBlocked("review-marker-third")
	})

	if got := strings.Count(out, "review-marker-first"); got != 1 {
		t.Fatalf("expected the first call's marker exactly once, got %d in: %s", got, out)
	}
	if strings.Contains(out, "review-marker-second") {
		t.Fatalf("second call must be suppressed by dedup, but its marker appeared: %s", out)
	}
	if strings.Contains(out, "review-marker-third") {
		t.Fatalf("third call must be suppressed by dedup, but its marker appeared: %s", out)
	}
}

// TestNotifyRequiredReceiveDiscardedFiresOnlyOnce is the receive-side analog.
func TestNotifyRequiredReceiveDiscardedFiresOnlyOnce(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	out := captureStderr(t, func() {
		sess.NotifyRequiredReceiveDiscarded("review-marker-rx-first")
		sess.NotifyRequiredReceiveDiscarded("review-marker-rx-second")
	})

	if got := strings.Count(out, "review-marker-rx-first"); got != 1 {
		t.Fatalf("expected the first call's marker exactly once, got %d in: %s", got, out)
	}
	if strings.Contains(out, "review-marker-rx-second") {
		t.Fatalf("second call must be suppressed by dedup, but its marker appeared: %s", out)
	}
}

// TestRequiredGateAlreadyCancelledCtxReturnsPromptly: a send whose sendPack.Ctx
// is already cancelled before it ever reaches the EncryptionModeRequired
// entry gate (cipher not established) must return promptly via the
// `case <-sendPack.Ctx.Done()` arm of the gate's select, not wait out a full
// poll interval or the caller's timeout budget.
func TestRequiredGateAlreadyCancelledCtxReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, b, _, bClientId, _, _ := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeOff, nil, true)
	defer a.Cancel()
	defer b.Cancel()

	packCtx, packCancel := context.WithCancel(context.Background())
	packCancel() // already done before the send is even attempted

	frame := requiredGateFrame(t, "already-cancelled")

	start := time.Now()
	success, err := a.SendWithTimeoutDetailed(
		frame,
		DestinationId(bClientId),
		func(error) {},
		10*time.Second, // budget far longer than a prompt return
		Ctx(packCtx),
	)
	elapsed := time.Since(start)

	if success {
		t.Fatal("a send with an already-cancelled Ctx must not succeed")
	}
	if err == nil {
		t.Fatal("expected an error for an already-cancelled Ctx")
	}
	if 2*time.Second <= elapsed {
		t.Fatalf("send with an already-cancelled Ctx took %s: the gate did not return promptly on Ctx.Done()", elapsed)
	}
}
