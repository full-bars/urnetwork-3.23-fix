//go:build linux

package urnettools

// Regression tests for the HotSwap dormancy bug.
//
// update swaps the provider binary on disk BEFORE running the HotSwap
// preflight. On Linux a rename over a running binary leaves
// /proc/<pid>/exe's TARGET naming a deleted file, so every read through
// that target fails. hotSwapVersionOK read through it, got nothing, fell
// back to p.Version (empty on every -trimpath release build), and declined
// with ErrHotSwapNotSupported — reporting an out-of-date provider no matter
// how new the running one was, and never reaching the Type= check at all.
//
// These tests pin the property that makes the fix work: the /proc/<pid>/exe
// symlink itself keeps resolving to the loaded inode after the swap, even
// though its target string does not.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startSwappedFakeProvider copies the test binary to dir/urnetwork, starts it
// as a stand-in provider reporting version, then renames a fresh copy over it
// to reproduce exactly what update's binary swap does. It returns the pid of
// the still-running process, whose on-disk image is now gone.
func startSwappedFakeProvider(t *testing.T, version string) int {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	payload, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatalf("write fake provider: %v", err)
	}

	// Set it on THIS process, not just the child: providerVersionFromExec
	// runs the binary with an inherited environment, so the --version probe
	// it spawns must land in helper mode too.
	t.Setenv(fakeProviderVersionEnv, version)

	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake provider: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the kernel to publish the exe link before swapping, so the
	// test cannot race the exec.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Readlink("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/exe"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake provider %d never published /proc/<pid>/exe", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The swap: rename a new file over the running binary, exactly as
	// installBinary does. The running process keeps the old inode.
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(staged, payload, 0o755); err != nil {
		t.Fatalf("write staged binary: %v", err)
	}
	if err := os.Rename(staged, bin); err != nil {
		t.Fatalf("swap binary: %v", err)
	}
	return cmd.Process.Pid
}

func TestRunningImagePathReportsDeletedAfterSwap(t *testing.T) {
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.31.0")

	got, err := runningImagePath(pid)
	if err != nil {
		t.Fatalf("runningImagePath: %v", err)
	}
	// This is the precondition that broke everything downstream. If the
	// kernel ever stopped marking the swapped image, the bug below would
	// vanish and this test should be revisited rather than deleted.
	if !strings.HasSuffix(got, " (deleted)") {
		t.Fatalf("expected a deleted marker after the swap, got %q", got)
	}
	if _, err := os.Stat(got); err == nil {
		t.Fatalf("readlink target %q is still readable; the swap did not reproduce", got)
	}
}

func TestRunningImageHandleStaysReadableAfterSwap(t *testing.T) {
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.31.0")

	handle, err := runningImageHandle(pid)
	if err != nil {
		t.Fatalf("runningImageHandle: %v", err)
	}
	if !isRecognizedExecutable(handle) {
		t.Fatalf("handle %q is not a recognized executable after the swap", handle)
	}
}

func TestProviderVersionReadsThroughHandleAfterSwap(t *testing.T) {
	const want = "v3.23.0-fix.31.0"
	pid := startSwappedFakeProvider(t, want)

	// The old code path: read through the readlink target. It must fail,
	// which is precisely why the version gate saw an empty string.
	stale, err := runningImagePath(pid)
	if err != nil {
		t.Fatalf("runningImagePath: %v", err)
	}
	if got := providerVersion(stale); got != "" {
		t.Fatalf("expected no version through the deleted target, got %q", got)
	}

	// The fixed path.
	handle, err := runningImageHandle(pid)
	if err != nil {
		t.Fatalf("runningImageHandle: %v", err)
	}
	if got := providerVersion(handle); got != want {
		t.Fatalf("providerVersion through handle = %q, want %q", got, want)
	}
}

func TestHotSwapVersionOKAfterBinarySwap(t *testing.T) {
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.31.0")

	// p.Version is empty here on purpose: that is the state discovery
	// reports for every -trimpath release build, so the running image is
	// the only source of truth left.
	p := Provider{PID: pid}
	if !hotSwapVersionOK(p) {
		t.Fatal("hotSwapVersionOK = false after a binary swap; HotSwap is dormant again")
	}
}

func TestHotSwapPreflightDoesNotBlameVersionAfterSwap(t *testing.T) {
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.31.0")

	origUnitType := unitTypeFunc
	defer func() { unitTypeFunc = origUnitType }()
	unitTypeFunc = func(Provider) (string, error) { return "simple", nil }

	err := hotSwapPreflight(Provider{PID: pid, Unit: "urnetwork.service"})
	if err == nil {
		t.Fatal("expected Type=simple to decline the handoff")
	}
	// The decline must name the real cause. Reporting the version reason for
	// a Type=simple unit is what hid the dormancy: operators read it as "my
	// provider is too old" and never looked at the unit type.
	if strings.Contains(err.Error(), "requires >=") {
		t.Fatalf("declined with the version reason for a Type=simple unit: %v", err)
	}
}

func TestHotSwapVersionOKRejectsOldRunningProvider(t *testing.T) {
	pid := startSwappedFakeProvider(t, "v3.23.0-fix.30.9")

	// The gate must still do its job: SIGUSR2 to a provider with no handler
	// kills it, so a genuinely old running image has to decline.
	if hotSwapVersionOK(Provider{PID: pid}) {
		t.Fatal("hotSwapVersionOK = true for a v30.9 running image")
	}
}
