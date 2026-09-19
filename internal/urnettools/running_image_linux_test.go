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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rewriteVersionStamp overwrites the FIRST embedded URNET_VERSION_STAMP=...
// value that actually looks like a release version (v-prefixed) with the
// requested version. Used by the fake-provider fixtures so the read-only
// version scanner deterministically resolves the fake version rather than
// the tool's own embedded one.
//
// Two constraints keep the rewritten payload a valid ELF:
//  1. Only v-prefixed values are rewritten — the binary also contains the
//     prefix as part of unrelated string data (e.g. "…=floating"), and
//     rewriting that would corrupt a string literal mid-file.
//  2. The replacement must be exactly the same byte length as the original
//     value: inserting or removing bytes in the middle of an ELF shifts
//     every section offset after it, producing a binary the loader cannot
//     run. All release versions in this suite are the same length
//     (v3.23.0-fix.XX.0), so the rewrite is a byte-for-byte overlay; when
//     the new version is shorter it is padded with spaces (a valid
//     terminator for the stamp scan) up to the original length, and when
//     longer the stamp is appended at the end instead — appended bytes after
//     the ELF image are ignored by the loader.
func rewriteVersionStamp(payload []byte, version string) []byte {
	prefix := []byte(versionStampPrefix)
	searchFrom := 0
	for {
		i := bytes.Index(payload[searchFrom:], prefix)
		if i < 0 {
			break
		}
		i += searchFrom
		start := i + len(prefix)
		end := len(payload)
		for j := start; j < len(payload); j++ {
			if payload[j] == ' ' || payload[j] == '\n' || payload[j] == 0 {
				end = j
				break
			}
		}
		old := payload[start:end]
		// Same rule as scanVersionStamp: a stamp value is 'v' then a digit.
		// A 'v'-then-letter span (the prefix constant sitting next to an
		// unrelated string in rodata) is not a stamp and must not be the one
		// overwritten, or the scanner would read a different, later stamp.
		if len(old) > 1 && old[0] == 'v' && old[1] >= '0' && old[1] <= '9' {
			// Byte-for-byte overlay within the original span.
			if len(version) <= len(old) {
				out := make([]byte, len(payload))
				copy(out, payload)
				copy(out[start:], version)
				for k := start + len(version); k < end; k++ {
					out[k] = ' ' // pad to original length; terminator for the scan
				}
				return out
			}
			// New version longer than the embedded slot: append at end. The
			// scanner returns the FIRST valid stamp, so the original must be
			// made invalid first (v -> x) or it would win over the appended one.
			out := make([]byte, len(payload))
			copy(out, payload)
			out[start] = 'x'
			return append(out, append([]byte("\n"+versionStampPrefix), []byte(version+"\n")...)...)
		}
		searchFrom = i + 1
	}
	// No v-prefixed stamp present: append one.
	return append(payload, append([]byte("\n"+versionStampPrefix), []byte(version+"\n")...)...)
}

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

	// Embed the version as the same raw byte marker release builds carry
	// (URNET_VERSION_STAMP=... via -ldflags -X). providerVersionReadOnly
	// resolves versions by scanning those bytes — it must NOT exec the
	// probed binary (exec'ing a discovered /proc/<pid>/exe would run an
	// attacker-chosen ELF as root).
	//
	// The test binary is itself a release-stamped build, so its embedded
	// stamp would otherwise win the scan and report the TOOL's version, not
	// the requested fake one. Rewrite any embedded stamp value to the
	// requested version so the read-only scanner deterministically resolves
	// `version`. Trailing bytes after the ELF image are ignored by the
	// dynamic loader, so the payload stays runnable.
	payload = rewriteVersionStamp(payload, version)

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
	if got := providerVersionReadOnly(stale); got != "" {
		t.Fatalf("expected no version through the deleted target, got %q", got)
	}

	// The fixed path.
	handle, err := runningImageHandle(pid)
	if err != nil {
		t.Fatalf("runningImageHandle: %v", err)
	}
	if got := providerVersionReadOnly(handle); got != want {
		t.Fatalf("providerVersionReadOnly through handle = %q, want %q", got, want)
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
