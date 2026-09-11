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

// TestRamLogPathsFor pins the per-provider path convention and its fallback
// order, which is what lets a multi-provider box avoid conflating buffers.
func TestRamLogPathsFor(t *testing.T) {
	got := ramLogPathsFor(Provider{Binary: "/opt/bin/urnetwork-b"})
	want := []string{"/dev/shm/urnetwork-b.log", "/dev/shm/urnetwork.log"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}

	got = ramLogPathsFor(Provider{})
	if len(got) != 1 || got[0] != "/dev/shm/urnetwork.log" {
		t.Errorf("no binary: got %v, want the shared path only", got)
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
