package urnettools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRamlogsEnvEnabled covers the systemd Environment= property as systemd
// actually renders it: one space-separated line, values quoted when needed.
// This is the input that the old code never looked at.
func TestRamlogsEnvEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"empty", "", false},
		{"unset key", "URNETWORK_PROFILE=turbo-v8 HOST_HOSTNAME=box", false},
		{"plain one", "URNETWORK_RAMLOGS=1", true},
		{"quoted one", `"URNETWORK_RAMLOGS=1"`, true},
		{"on", "URNETWORK_RAMLOGS=on", true},
		{"true", "URNETWORK_RAMLOGS=true", true},
		{"yes", "URNETWORK_RAMLOGS=yes", true},
		{"uppercase value", "URNETWORK_RAMLOGS=ON", true},
		{"zero", "URNETWORK_RAMLOGS=0", false},
		{"off", "URNETWORK_RAMLOGS=off", false},
		{"empty value", "URNETWORK_RAMLOGS=", false},
		{"among others", `HOST_HOSTNAME=box "URNETWORK_RAMLOGS=1" URNETWORK_PROFILE=turbo-v8`, true},
		{"trailing newline", "URNETWORK_RAMLOGS=1\n", true},
		{"prefix must not match", "URNETWORK_RAMLOGS_EXTRA=1", false},
		{"suffix must not match", "MY_URNETWORK_RAMLOGS=1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ramlogsEnvEnabled(c.env); got != c.want {
				t.Errorf("ramlogsEnvEnabled(%q) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestRamlogsEnvEnabledOverride verifies that the last assignment to
// URNETWORK_RAMLOGS wins, matching systemd drop-in overlay semantics.
func TestRamlogsEnvEnabledOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{
			"last wins on",
			"URNETWORK_RAMLOGS=0 URNETWORK_RAMLOGS=1",
			true,
		},
		{
			"last wins off",
			"URNETWORK_RAMLOGS=1 URNETWORK_RAMLOGS=0",
			false,
		},
		{
			"drop-in overrides unit body",
			"URNETWORK_RAMLOGS=on URNETWORK_PROFILE=turbo-v8 URNETWORK_RAMLOGS=off",
			false,
		},
		{
			"drop-in enables over disabled body",
			`HOST_HOSTNAME=box URNETWORK_RAMLOGS=0 "URNETWORK_RAMLOGS=1"`,
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ramlogsEnvEnabled(c.env); got != c.want {
				t.Errorf("ramlogsEnvEnabled(%q) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestRamlogsEnvEnabledQuotedValues covers quoted values with spaces and
// embedded equals signs, which systemd produces for complex Environment= lines.
func TestRamlogsEnvEnabledQuotedValues(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{
			"quoted with spaces",
			`HOST_HOSTNAME=my box URNETWORK_RAMLOGS=1`,
			true,
		},
		{
			"double-quoted on",
			`HOST_HOSTNAME="my box" URNETWORK_RAMLOGS="1"`,
			true,
		},
		{
			"single-quoted on",
			`HOST_HOSTNAME='my box' URNETWORK_RAMLOGS='true'`,
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ramlogsEnvEnabled(c.env); got != c.want {
				t.Errorf("ramlogsEnvEnabled(%q) = %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestRamLogPathsFor pins the per-provider path convention and its fallback
// order, which is what lets a multi-provider box avoid conflating buffers.
func TestRamLogPathsFor(t *testing.T) {
	got := ramLogPathsFor(Provider{Binary: "/opt/bin/urnetwork-b"})
	want := []string{"/dev/shm/urnetwork-b.log"}
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	got = ramLogPathsFor(Provider{})
	if len(got) != 1 || got[0] != "/dev/shm/urnetwork.log" {
		t.Errorf("no binary: got %v, want the shared path only", got)
	}
}

// TestRamLogPathsForBinaryDedup verifies that when the binary basename is
// "urnetwork", the legacy shared path is not duplicated.
func TestRamLogPathsForBinaryDedup(t *testing.T) {
	got := ramLogPathsFor(Provider{Binary: "/opt/bin/urnetwork"})
	want := []string{"/dev/shm/urnetwork.log"}
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRamLogPathsForDirectoryBinary verifies that a binary path ending in /
// (a directory) is handled gracefully by filepath.Base.
func TestRamLogPathsForDirectoryBinary(t *testing.T) {
	got := ramLogPathsFor(Provider{Binary: "/opt/bin/"})
	if len(got) == 0 {
		t.Fatal("expected at least one path")
	}
	// filepath.Base("/opt/bin/") returns "bin", so the per-binary path
	// should be /dev/shm/bin.log.
	if got[0] != "/dev/shm/bin.log" {
		t.Errorf("got[0] = %q, want /dev/shm/bin.log", got[0])
	}
}

// TestRamLogPathsForNoBinary verifies that an empty binary only returns the
// legacy shared path.
func TestRamLogPathsForNoBinary(t *testing.T) {
	got := ramLogPathsFor(Provider{Binary: ""})
	if len(got) != 1 || got[0] != "/dev/shm/urnetwork.log" {
		t.Errorf("got %v, want [/dev/shm/urnetwork.log]", got)
	}
}

// ramlogFileActive stats absolute /dev/shm paths, so these tests exercise the
// decision rules through a seam rather than writing to the host's shared
// memory. The rules are what matter: running, non-empty, recently modified.
func TestRamlogFileActiveRules(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.log")
	if err := os.WriteFile(fresh, []byte("a log line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(dir, "stale.log")
	if err := os.WriteFile(stale, []byte("an old line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		path    string
		running bool
		want    bool
	}{
		{"fresh and running", fresh, true, true},
		{"fresh but stopped", fresh, false, false},
		{"empty file", empty, true, false},
		{"stale file", stale, true, false},
		{"missing file", filepath.Join(dir, "nope.log"), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ramlogFileFresh(c.path, c.running); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestRamlogFileFreshFutureModTime verifies that a file with a future
// modification time (clock skew) is not treated as fresh.
func TestRamlogFileFreshFutureModTime(t *testing.T) {
	dir := t.TempDir()
	future := filepath.Join(dir, "future.log")
	if err := os.WriteFile(future, []byte("data\n"), 0644); err != nil {
		t.Fatal(err)
	}
	futureTime := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(future, futureTime, futureTime); err != nil {
		t.Fatal(err)
	}
	if got := ramlogFileFresh(future, true); got {
		t.Error("future modtime file should not be fresh")
	}
}

// TestUnitEnvironmentNoUnit asserts a provider with no owning unit is not an
// error case. Docker-managed and bare-process providers reach here.
func TestUnitEnvironmentNoUnit(t *testing.T) {
	env, err := unitEnvironment(Provider{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != "" {
		t.Errorf("got %q, want empty", env)
	}
}
