package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestShortID pins the display truncation used by cmdProviders' NET-ID
// column: short ids pass through untouched, long ones truncate to 8 chars
// plus an ellipsis so the table stays aligned.
func TestShortID(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"", ""},
		{"short", "short"},
		{"exactly8", "exactly8"},
		{"123456789", "12345678…"},
		{"a0b1c2d3-e4f5-6789-abcd-ef0123456789", "a0b1c2d3…"},
	}
	for _, c := range cases {
		if got := shortID(c.id); got != c.want {
			t.Errorf("shortID(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestCmdProvidersRuns exercises cmdProviders end to end against whatever
// this host's real Discover() returns — the empty-fleet branch (print the
// friendly message) on a box with nothing running, or the table-print
// branch otherwise. Purely read-only (fmt.Println/tabwriter to stdout), so
// safe regardless of which branch the host takes.
func TestCmdProvidersRuns(t *testing.T) {
	if err := cmdProviders(nil); err != nil {
		t.Errorf("cmdProviders() = %v, want nil", err)
	}
}

// TestCmdSimpleDelegationNoProviders confirms the pass-through commands
// (summary/report/hot-restart) surface a targeting error rather than
// panicking when nothing is discovered on the box. Skips on a host where a
// real provider binary is discoverable: cmdSimpleDelegation would delegate
// "summary" to that binary as a live subprocess, which this unit test must
// never trigger.
func TestCmdSimpleDelegationNoProviders(t *testing.T) {
	for _, p := range Discover() {
		if p.Binary != "" {
			t.Skip("host has a live provider binary; delegation would exec a real subprocess")
		}
	}
	err := cmdSimpleDelegation("summary", nil)
	if err == nil {
		t.Fatal("cmdSimpleDelegation with no resolvable provider binary must error")
	}
}

// TestInstallBinarySuccess drives installBinary's full happy path: copy to
// dst+".new", chmod 0755, atomic rename over dst. user is empty so the
// os/exec chown branch (which requires root) is skipped, matching how the
// function behaves for the common non-root operator flow.
func TestInstallBinarySuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "provider-src")
	if err := os.WriteFile(src, []byte("fake binary contents"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(dir, "provider")
	// Pre-existing dst must be replaced by the rename, not merged with.
	if err := os.WriteFile(dst, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	if err := installBinary(src, dst, ""); err != nil {
		t.Fatalf("installBinary() = %v, want nil", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after install: %v", err)
	}
	if string(got) != "fake binary contents" {
		t.Errorf("dst content = %q, want the new binary's content", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("dst mode = %v, want 0755 (Windows reports 0666; no POSIX perms)", info.Mode().Perm())
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Errorf("dst+.new should have been renamed away, stat err = %v", err)
	}
}

// TestInstallBinaryMissingSource confirms installBinary surfaces the copy
// error (rather than a nil error or a partially-written dst) when the
// staged source file does not exist.
func TestInstallBinaryMissingSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist")
	dst := filepath.Join(dir, "provider")

	if err := installBinary(src, dst, ""); err == nil {
		t.Fatal("installBinary with a missing source must error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst must not be created on a failed install, stat err = %v", err)
	}
}

// TestRestartAfterDropinSystemUnitPropagatesError exercises the system-unit
// branch of restartAfterDropin against a unit that cannot exist, pinning
// systemctl's failure must reach the caller.
// A fake unit name reliably errors on any Linux host, so err==nil is a
// hard failure — NOT a pass-through log (the
// t.Log form would stay green if the fix were reverted to
// `_ = ...Run(); return nil`).
func TestRestartAfterDropinSystemUnitPropagatesError(t *testing.T) {
	p := Provider{Unit: "urnet-tools-test-fake-unit-r2.service"}
	err := restartAfterDropin(p)
	if err == nil {
		t.Fatal("restartAfterDropin must propagate systemctl restart error for a fake unit (MEDIUM-2)")
	}
}

// TestRestartAfterDropinUserUnitPropagatesError exercises the user-unit
// branch of restartAfterDropin (isUserUnit + p.User set), which is exactly
// the branch the round-2 fix changed from `_ = ...Run(); return nil` to
// returning the restart error directly. Same hard-fail discipline: a fake
// user unit cannot exist, so nil means the error was swallowed (regression).
func TestRestartAfterDropinUserUnitPropagatesError(t *testing.T) {
	p := Provider{Unit: "urnet-tools-test-fake-user-unit-r2.service", User: "urnet-tools-test-fake-user-r2"}
	err := restartAfterDropin(p)
	if err == nil {
		t.Fatal("restartAfterDropin (user branch) must propagate systemctl restart error for a fake unit (MEDIUM-2)")
	}
}
