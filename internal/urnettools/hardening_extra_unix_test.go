//go:build unix

package urnettools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO with no writer must be rejected promptly: os.Stat accepted it and
// the later os.ReadFile blocked forever waiting for input.
func TestReadSessionLoadFileRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "bundle.enc")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readSessionLoadFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO: got %v, want a not-a-regular-file error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readSessionLoadFile blocked on a FIFO")
	}
}

// A command that never produces output or exits (cat on a FIFO) must be
// killed at the deadline instead of blocking the bounded read forever.
func TestRunCappedTimeoutKillsStuckCommand(t *testing.T) {
	start := time.Now()
	_, _, err := runCappedTimeout(exec.Command("sleep", "30"), 1024, 200*time.Millisecond)
	if !errors.Is(err, errCommandTimeout) {
		t.Fatalf("got %v, want errCommandTimeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; the deadline did not unblock the read", d)
	}
	// A command that finishes in time is unaffected.
	out, _, err := runCappedTimeout(exec.Command("sh", "-c", "printf ok"), 1024, 5*time.Second)
	if err != nil || string(out) != "ok" {
		t.Fatalf("fast command: %q, %v", out, err)
	}
}

// writeStateFileOwned writes through one descriptor and applies the owner on
// it; with the caller already owning ownerDir it is a plain write.
func TestWriteStateFileOwnedWritesAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	if err := writeStateFileOwned(dir, "direct", []byte("1\n"), 0o600, dir); err != nil {
		t.Fatalf("writeStateFileOwned: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "direct"))
	if err != nil || string(b) != "1\n" {
		t.Fatalf("got %q, %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "direct"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	if err := writeStateFileOwned(t.TempDir(), "x", []byte("y"), 0o600, filepath.Join(dir, "missing")); err == nil {
		t.Error("an unresolvable owner dir must be an error, not a silent skip")
	}
}

// The selected provider's own state dir or user home wins; the caller's home
// is never consulted (under sudo that is /root).
func TestProviderMarkerPath(t *testing.T) {
	if got := providerMarkerPath(Provider{StateDir: "/srv/u/.urnetwork"}); got != "/srv/u/.urnetwork/disable_ip_autodetect" {
		t.Errorf("state dir: got %q", got)
	}
	if got := providerMarkerPath(Provider{}); got != "" {
		t.Errorf("nothing resolvable: got %q, want empty", got)
	}
	t.Setenv("HOME", "/should/not/be/used")
	if got := providerMarkerPath(Provider{User: "no-such-user-zzz"}); got != "" {
		t.Errorf("unknown user must not fall back to the caller's home: got %q", got)
	}
	if home := homeForUser("root"); home != "" {
		if got := providerMarkerPath(Provider{User: "root"}); got != home+"/.urnetwork/disable_ip_autodetect" {
			t.Errorf("user home: got %q", got)
		}
	}
}
