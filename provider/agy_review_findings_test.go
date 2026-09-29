package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// critLog appended its own newline, so a deferred line drained with a trailing
// "\n" in the format left a blank line in events.log for every warning.
func TestDeferredCritWriteDoesNotDoubleTheNewline(t *testing.T) {
	withTempHome(t)
	deferCritWrite("a deferred warning")
	lines := drainDeferredCrit()
	if len(lines) != 1 || lines[0] != "a deferred warning" {
		t.Fatalf("drain returned %q, want the queued line unchanged", lines)
	}
	// critLog writes to events.log, not stdout, so assert on the file. The
	// drain sites format without a trailing newline since critLog adds one.
	critLog("%s", lines[0])
	lp, err := critLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n\n") {
		t.Fatalf("draining a line must not leave a blank line, got %q", string(b))
	}
	if !strings.Contains(string(b), "a deferred warning") {
		t.Fatalf("the line must still reach events.log, got %q", string(b))
	}
}

// Draining empties the queue, so a line cannot be written twice.
func TestDrainDeferredCritEmptiesTheQueue(t *testing.T) {
	deferCritWrite("once")
	if got := drainDeferredCrit(); len(got) != 1 {
		t.Fatalf("first drain = %q, want one line", got)
	}
	if got := drainDeferredCrit(); len(got) != 0 {
		t.Fatalf("second drain = %q, want empty", got)
	}
}

// An unreadable proxy_trim used to log unconditionally, so a repeat of the same
// error wrote a bare newline to the ramlog and events.log on every reload.
func TestUnreadableProxyTrimWarnsOnceWithoutBlankLines(t *testing.T) {
	withTempHome(t)
	trimUnreadableReset()
	dir, _ := oomCapDir()
	_ = os.MkdirAll(dir, 0o700)
	trimPath, _ := proxyTrimPath()
	if err := os.MkdirAll(trimPath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "on")
	drainDeferredCrit() // start from an empty queue

	// First sighting warns and queues exactly one line.
	out := captureTlog(t, func() { effectiveTrimCapSource() })
	if !strings.Contains(out, "cannot read proxy_trim") {
		t.Fatalf("the first sighting must warn, got %q", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Fatalf("a warning must not be followed by a blank line, got %q", out)
	}
	if got := drainDeferredCrit(); len(got) != 1 {
		t.Fatalf("first sighting must queue exactly one line, got %q", got)
	}

	// A repeat of the same error says nothing at all, not even a blank line.
	repeat := captureTlog(t, func() { effectiveTrimCapSource() })
	if repeat != "" {
		t.Fatalf("a repeat must be completely silent, got %q", repeat)
	}
	if got := drainDeferredCrit(); len(got) != 0 {
		t.Fatalf("a repeat must queue nothing, got %q", got)
	}
}

// The unreadable-file warning is produced inside reload() while r.mu is held,
// so its events.log write must be deferred like the trim and ledger writes.
func TestUnreadableProxyTrimWritesEventsLogOutsideTheReloaderLock(t *testing.T) {
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	trimUnreadableReset()
	t.Cleanup(trimUnreadableReset)
	r, _, _ := trimFixture(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the file belongs: os.ReadFile returns EISDIR.
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork", "proxy_trim"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_OOM_CAP", "on")

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
		t.Fatal("the unreadable cap produced no events.log write")
	}
	if heldDuring != 0 {
		t.Fatalf("%d of %d events.log writes ran while reload held r.mu", heldDuring, calls)
	}
}

// A provider in a container capped below the 300 MiB reserve floor used to be
// told it had a NEGATIVE amount left after the reserve, which reads as broken.
func TestRAMReserveNeverExceedsTheBox(t *testing.T) {
	cases := []struct{ ram, want int64 }{
		{256, 256}, // below the floor: reserve what the box has
		{300, 300},
		{1024, 300},
	}
	for _, c := range cases {
		got := ramReserve(c.ram*testMiB) >> 20
		if got != c.want {
			t.Errorf("ramReserve(%d MiB) = %d MiB, want %d MiB", c.ram, got, c.want)
		}
	}
	// And the message must never carry a negative remainder.
	got := strings.Join(resourceConfigWarnings(resourceConfigInput{
		Proxies:           200,
		EffectiveRAMBytes: 256 * testMiB,
	}), " | ")
	if strings.Contains(got, "-0 MiB left") || strings.Contains(got, "(-") {
		t.Fatalf("the warning must not report a negative remainder, got %q", got)
	}
	if !strings.Contains(got, "0 MiB left") {
		t.Fatalf("a 256 MiB box should report nothing left after the reserve, got %q", got)
	}
}
