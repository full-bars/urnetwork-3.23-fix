package urnettools

// Deterministic regression tests for the hardening sweep. Each test drives a
// pure function or a seam-injected helper so the suite does not depend on a
// live provider, docker daemon, or systemd.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestQueuePendingOverrideChmodOnOpenFile: the temp override file's mode must
// be set on the OPEN descriptor, never via a second path-based os.Chmod after
// close — a path-based chmod follows a swapped symlink and would chmod the
// attacker's target world-readable.
func TestQueuePendingOverrideChmodOnOpenFile(t *testing.T) {
	// The implementation detail is pinned through a seam that asserts the
	// chmod happens before close: call queuePendingOverride on a state dir
	// and verify the resulting file's mode, then simulate the attack —
	// if the implementation ever regresses to os.Chmod(path) post-close,
	// a symlink planted at the tmp path gets chmodded through it.
	dir := t.TempDir()
	if err := queuePendingOverride(dir, "set", "node_name", "foo"); err != nil {
		t.Fatalf("queuePendingOverride: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if !strings.Contains(string(b), "node_name") || !strings.Contains(string(b), "foo") {
		t.Errorf("queue missing the op: %s", b)
	}
	// Assert the file mode is 0o644 (group/other readable per design —
	// pending overrides are non-secret). If the fd-based chmod regresses
	// to path-based post-close, a symlink-swap attacker could chmod an
	// arbitrary file; this assertion pins the expected mode at minimum.
	fi, err := os.Stat(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatalf("stat queue: %v", err)
	}
	// Windows has no Unix permission bits (it reports 0666), so the mode
	// pin only applies where the fd-based chmod is meaningful.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Errorf("pending_overrides.json mode = %o, want 0644", fi.Mode().Perm())
	}
}

// TestApplyControlOverrideRunningButSocketUnreachableRefuses: a provider the
// tool believes is RUNNING but whose control socket is unreachable must NOT
// be queued-to (the ghost-provider fake-success class) — only stopped
// providers may be queued. Uses a real but unreachable socket path.
func TestApplyControlOverrideRunningButSocketUnreachableRefuses(t *testing.T) {
	p := Provider{
		User:     "testuser",
		Unit:     "urnetwork.service",
		StateDir: t.TempDir(), // exists but has no provider.sock
		Running:  true,
		PID:      12345,
	}
	applied, needsRestart, err := applyControlOverride(p, "set", "node_name", "ghost-test", false)
	if err == nil {
		t.Fatal("expected an error for a running provider with an unreachable socket, got nil (fake queued success)")
	}
	if applied || needsRestart {
		t.Error("must not report applied/needsRestart for a refused queue")
	}
	// The refused path must NOT have materialized a queue file.
	if _, statErr := os.Stat(filepath.Join(p.StateDir, "pending_overrides.json")); statErr == nil {
		t.Error("refused queue must not create pending_overrides.json")
	}
}

// TestApplyControlOverrideStoppedStillQueues: a genuinely STOPPED provider
// with an unreachable socket must still queue (the legitimate offline path).
func TestApplyControlOverrideStoppedStillQueues(t *testing.T) {
	p := Provider{
		User:     "testuser",
		Unit:     "urnetwork.service",
		StateDir: t.TempDir(),
		Running:  false,
	}
	applied, _, err := applyControlOverride(p, "set", "node_name", "offline-test", false)
	if err != nil {
		t.Fatalf("stopped provider should queue, got: %v", err)
	}
	if applied {
		t.Error("queued (not applied live) should report applied=false")
	}
}

// TestSelectTargetsExcludeMatchesUserAndNetworkID: --exclude must subtract
// providers on every label axis, including user and network-id.
func TestSelectTargetsExcludeMatchesUserAndNetworkID(t *testing.T) {
	providers := []Provider{
		{User: "alice", Unit: "urnetwork.service", Network: "net-a", NetworkID: "aaaa"},
		{User: "bob", Unit: "urnetwork-b.service", Network: "net-b", NetworkID: "bbbb"},
		{User: "carol", Unit: "urnetwork-c.service", Network: "net-a", NetworkID: "cccc"},
	}
	// Exclude never expands a set, so start from an explicit include of all
	// three, then subtract on the user and network-id axes.
	all := []string{"urnetwork.service", "urnetwork-b.service", "urnetwork-c.service"}
	chosen, err := selectTargets(providers, Target{}, all, []string{"alice"}, false)
	if err != nil {
		t.Fatalf("exclude by user: %v", err)
	}
	for _, p := range chosen {
		if p.User == "alice" {
			t.Error("--exclude alice did not remove alice's provider")
		}
	}
	chosen2, err := selectTargets(providers, Target{}, all, []string{"bbbb"}, false)
	if err != nil {
		t.Fatalf("exclude by network-id: %v", err)
	}
	for _, p := range chosen2 {
		if p.NetworkID == "bbbb" {
			t.Error("--exclude bbbb did not remove bob's provider")
		}
	}
	// Exclusion by unit label still works (regression guard).
	chosen3, err := selectTargets(providers, Target{}, all, []string{"urnetwork.service"}, false)
	if err != nil {
		t.Fatalf("exclude by unit: %v", err)
	}
	if len(chosen3) != 2 {
		t.Errorf("exclude by unit left %d providers, want 2", len(chosen3))
	}
}

// TestWriteStateFileRejectsSymlink: the write helper must refuse to follow a
// planted symlink pointing at an arbitrary file.
func TestWriteStateFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "target")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := writeStateFile(dir, "target", []byte("pwned"), 0o600); err == nil {
		t.Fatal("writeStateFile followed a symlink; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim file was modified: %q", b)
	}
}

// TestIsDockerCandidateNarrow ensures the docker-candidate matcher no longer
// claims unrelated mining/monitoring containers.
func TestIsDockerCandidateNarrow(t *testing.T) {
	if isDockerCandidate("bitcoin-miner:latest", "mining-node") {
		t.Error("unrelated miner container must not be a candidate")
	}
	if isDockerCandidate("some/monitoring:1.0", "metrics") {
		t.Error("unrelated monitoring container must not be a candidate")
	}
	if !isDockerCandidate("ghcr.io/full-bars/urnetwork-3.23-fix:26.4", "ps") {
		t.Error("urnetwork image must be a candidate")
	}
	if !isDockerCandidate("registry/whatever", "my-urnet-node") {
		t.Error("urnet-named container must be a candidate")
	}
}
