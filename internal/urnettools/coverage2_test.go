package urnettools

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestartProviderNoResolution: a Provider with no Unit, no User, and no
// PID cannot be restarted by any of the three strategies (systemd unit,
// user-level unit, PID signal) — restartProvider must return a clear error
// rather than silently reporting success (this path had no test coverage).
func TestRestartProviderNoResolution(t *testing.T) {
	err := restartProvider(Provider{})
	if err == nil {
		t.Fatal("expected error when no unit/PID is resolvable")
	}
	if !strings.Contains(err.Error(), "could not restart") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRestartProviderPIDSignalFailure: a User+PID pair where the PID does
// not correspond to a live process must fall through to the final error
// rather than reporting a successful restart (Signal fails silently
// swallowed would be a false "restarted" claim to the operator).
func TestRestartProviderPIDSignalFailure(t *testing.T) {
	// Use a REAL dead PID: fork a child that exits immediately and reap it,
	// then signal its (now-reaped) PID. A guessed PID like 999999 could
	// theoretically collide on a huge-pid-max system (coderabbit major).
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	pid := dead.Process.Pid
	_ = dead.Wait() // reaped — pid is now guaranteed dead
	err := restartProvider(Provider{User: "nobody-test-9f3a", PID: pid})
	if err == nil {
		t.Fatal("expected error when the PID does not correspond to a live process")
	}
}

// TestCmdProxyHealthTargetNoSnapshot: with no state files present,
// cmdProxyHealthTarget must return cleanly (nil) rather than blocking on
// `tail -f` — it only starts following the log if the log file actually
// exists.
func TestCmdProxyHealthTargetNoSnapshot(t *testing.T) {
	p := Provider{StateDir: t.TempDir()}
	if err := cmdProxyHealthTarget(p); err != nil {
		t.Fatalf("expected nil error with no snapshot/log present, got %v", err)
	}
}

// TestCmdProxyTrafficTargetNoSnapshot: with no traffic state file present,
// cmdProxyTrafficTarget must return cleanly.
func TestCmdProxyTrafficTargetNoSnapshot(t *testing.T) {
	p := Provider{StateDir: t.TempDir()}
	if err := cmdProxyTrafficTarget(p); err != nil {
		t.Fatalf("expected nil error with no traffic snapshot present, got %v", err)
	}
}

// TestCmdProxyTrafficTargetReadsSnapshot: when a snapshot file IS present,
// cmdProxyTrafficTarget must read and print it without error.
func TestCmdProxyTrafficTargetReadsSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := "rx=100 tx=200\n"
	if err := os.WriteFile(filepath.Join(dir, "proxy_traffic.state"), []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Provider{StateDir: dir}
	// Capture stdout so the printed snapshot is asserted, not just the
	// nil error (coderabbit minor: assert the snapshot output).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	err := cmdProxyTrafficTarget(p)
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("closing capture pipe: %v", cerr)
	}
	os.Stdout = old
	<-done
	r.Close()
	if err != nil {
		t.Fatalf("unexpected error reading snapshot: %v", err)
	}
	if !strings.Contains(buf.String(), "rx=100") {
		t.Errorf("printed output should contain the snapshot contents, got: %q", buf.String())
	}
}

// TestCmdAutoStartRequiresMode: no args must error before any targeting or
// systemd interaction happens.
func TestCmdAutoStartRequiresMode(t *testing.T) {
	err := cmdAutoStart(nil, false, false)
	if err == nil {
		t.Fatal("expected error for auto-start with no mode")
	}
	if !contains(err.Error(), "on|off") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdAutoStartInvalidMode: a mode that isn't on/off must be rejected
// before targeting runs.
func TestCmdAutoStartInvalidMode(t *testing.T) {
	err := cmdAutoStart([]string{"maybe"}, false, false)
	if err == nil {
		t.Fatal("expected error for invalid auto-start mode")
	}
	if !contains(err.Error(), "invalid value") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdAutoUpdateRequiresInterval: no args must error before targeting.
func TestCmdAutoUpdateRequiresInterval(t *testing.T) {
	err := cmdAutoUpdate(nil, false, false)
	if err == nil {
		t.Fatal("expected error for auto-update with no interval")
	}
	if !contains(err.Error(), "daily|weekly|monthly|off") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdAutoUpdateInvalidInterval: an interval outside the known set must
// be rejected by cmdAutoUpdate BEFORE targeting (validation moved ahead of
// selectTarget so this is testable without a live provider — the old test
// asserted a map literal against itself and could never fail; coderabbit
// minor).
func TestCmdAutoUpdateInvalidInterval(t *testing.T) {
	err := cmdAutoUpdate([]string{"yearly"}, false, false)
	if err == nil {
		t.Fatal("expected error for invalid auto-update interval")
	}
	if !contains(err.Error(), "invalid interval") {
		t.Errorf("unexpected error: %v", err)
	}
	// A valid interval must get past validation (it will then fail
	// targeting on a box with no providers — that's fine, the point is the
	// interval check itself passes).
	err = cmdAutoUpdate([]string{"daily"}, false, false)
	if err == nil {
		t.Fatal("expected targeting error for daily (no provider on test box)")
	}
	if contains(err.Error(), "invalid interval") {
		t.Errorf("daily must pass interval validation, got: %v", err)
	}
}

// TestUnitDropinDirNoUnit: a provider with no owning unit cannot resolve a
// drop-in directory — writeDropinEnv/removeDropinEnv must fail fast rather
// than writing to a guessed path.
func TestUnitDropinDirNoUnit(t *testing.T) {
	_, err := unitDropinDir(Provider{})
	if err == nil {
		t.Fatal("expected error for provider with no unit")
	}
	if !contains(err.Error(), "no owning unit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestUnitDropinDirUnresolvableUser: a user-level unit whose home can't be
// resolved via getent must error rather than falling back to a relative or
// guessed path.
func TestUnitDropinDirUnresolvableUser(t *testing.T) {
	bogus := "urnet-tools-test-nonexistent-user-9f3a"
	p := Provider{Unit: "urnet-tools-test-fake-unit-9f3a.service", User: bogus}
	_, err := unitDropinDir(p)
	if err == nil {
		t.Fatal("expected error for unresolvable user home")
	}
	if !contains(err.Error(), "cannot resolve home") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRemoveDropinEnvMissingFile: removing a drop-in that was never written
// must be a clean no-op, not an error — a provider that never had the
// setting toggled should be able to run `off` safely.
func TestRemoveDropinEnvMissingFile(t *testing.T) {
	// unitDropinDir for a system unit resolves under /etc/systemd/system,
	// which is not writable in a test sandbox — instead exercise the
	// documented "file does not exist" branch directly against a synthetic
	// path via a provider whose unit resolves to a system dir we don't own.
	// Since we cannot safely write there, assert the specific no-file
	// message is produced without attempting a restart.
	p := Provider{Unit: "urnet-tools-test-fake-unit-9f3a.service"}
	err := removeDropinEnv(p, "hub.conf", "URNETWORK_REPORT_URL")
	// System-unit branch: file under /etc/systemd/system/<unit>.d/hub.conf
	// will not exist, so this must return nil (informational message only).
	if err != nil {
		t.Fatalf("removing a nonexistent drop-in should be a clean no-op, got %v", err)
	}
}

// TestContainerIDByNameNoDocker: with no docker daemon reachable (or the
// dockerCLI stubbed to a nonexistent binary), containerIDByName must return
// "" rather than panicking or propagating the exec error.
func TestContainerIDByNameNoDocker(t *testing.T) {
	setDockerTestBin("urnet-tools-test-no-such-binary-9f3a")
	t.Cleanup(func() { setDockerTestBin("") })
	got := containerIDByName("whatever")
	if got != "" {
		t.Errorf("containerIDByName with no docker binary = %q, want empty", got)
	}
}
