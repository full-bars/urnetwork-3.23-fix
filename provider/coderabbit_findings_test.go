package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An unreadable state file must still warn. oomReadJSONChecked used to treat
// every os.Stat error as "the file is not there", so a stat failure that is not
// ENOENT (a parent directory the provider cannot search, a broken disk, a path
// component that is not a directory) silently dropped the warning about a
// standing cap or an OOM kill that is NOT being applied. The check exists to
// say exactly that, so a stat we could not perform is not evidence of absence.
func TestOOMReadJSONCheckedWarnsWhenStatFailsForAnotherReason(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	// A regular file where the DIRECTORY should be: oom_cap.json's parent path
	// component is a file, so os.Stat on the child fails with ENOTDIR, which is
	// not IsNotExist.
	blocker := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	under := filepath.Join(blocker, "oom_cap.json")

	var st oomCapState
	ok, warning := oomReadJSONChecked(under, &st)
	if ok {
		t.Fatal("an unreadable file must not report ok")
	}
	if warning == "" {
		t.Fatalf("a stat failure that is not ENOENT must still warn; %s is there but unreadable", filepath.Base(under))
	}
	if !strings.Contains(warning, "exists but could not be read") {
		t.Fatalf("warning = %q, want the unreadable-state wording", warning)
	}
}

// A genuinely missing file is normal (first start) and must stay silent, or the
// warning becomes noise nobody reads.
func TestOOMReadJSONCheckedStaysSilentForAMissingFile(t *testing.T) {
	withTempHome(t)
	dir, _ := oomCapDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var st oomCapState
	if ok, warning := oomReadJSONChecked(filepath.Join(dir, "oom_cap.json"), &st); ok || warning != "" {
		t.Fatalf("a missing file is normal and must be silent, got ok=%v warning=%q", ok, warning)
	}
}

// The invalid-cap warning used to key its dedup on a value truncated to 40
// bytes, so two different long values sharing a first 40 bytes collided and the
// second one never warned. It also stored only the LAST value, so an A, B, A
// sequence warned twice for A. Key on the full value, truncate only to format.
func TestTrimGarbageDedupsOnTheFullValue(t *testing.T) {
	trimGarbageReset()
	shared := strings.Repeat("x", 40)
	a := shared + "-alpha"
	b := shared + "-beta"

	if w := trimGarbageWarning(a); w == "" {
		t.Fatal("the first value must warn")
	}
	if w := trimGarbageWarning(b); w == "" {
		t.Fatal("two values sharing a 40-byte prefix must both warn; the truncated key collided")
	}
	if w := trimGarbageWarning(a); w != "" {
		t.Fatalf("a repeat of the first value must stay silent, got %q", w)
	}
	if w := trimGarbageWarning(b); w != "" {
		t.Fatalf("a repeat of the second value must stay silent, got %q", w)
	}
	// The message still truncates, so a huge file does not flood events.log.
	msg := trimGarbageWarning(shared + "-gamma")
	if !strings.Contains(msg, "...") {
		t.Fatalf("a long value must be truncated in the message, got %q", msg)
	}
}

// The same value reappearing after a different one must not warn again: a
// last-value-only store re-warned on every A, B, A cycle.
func TestTrimGarbageDoesNotRewarnOnAnABCycle(t *testing.T) {
	trimGarbageReset()
	if w := trimGarbageWarning("alpha"); w == "" {
		t.Fatal("alpha must warn first")
	}
	if w := trimGarbageWarning("bravo"); w == "" {
		t.Fatal("bravo must warn")
	}
	if w := trimGarbageWarning("alpha"); w != "" {
		t.Fatalf("alpha must not warn again after bravo, got %q", w)
	}
}

// The invalid-cap warning is written to events.log from inside reload(), which
// holds r.mu. That write is an open+write+fsync, so a slow disk blocked every
// other reload-path caller. The ramlog line stays immediate; the disk write is
// queued and drained after the lock is released. Asserted from inside critLog
// (the same test seam the events.log test uses), and the line must still land.
func TestTrimGarbageWarningWritesEventsLogOutsideTheReloaderLock(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	trimGarbageReset()
	t.Cleanup(trimGarbageReset)
	r, _, _ := trimFixture(t)

	// A proxy_trim holding something that is not a number.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "proxy_trim"), []byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls, heldDuring int
	critLogInCriticalSection = func() {
		calls++
		if r.mu.TryLock() {
			r.mu.Unlock()
			return
		}
		heldDuring++
	}
	t.Cleanup(func() { critLogInCriticalSection = nil })

	r.reload()

	if calls == 0 {
		t.Fatal("the invalid cap produced no events.log write")
	}
	if heldDuring != 0 {
		t.Fatalf("%d of %d events.log writes ran while reload held r.mu", heldDuring, calls)
	}
	p, err := critLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "not a proxy count") {
		t.Fatalf("events.log must still get the invalid-cap warning: err=%v content=%q", err, string(b))
	}
}
