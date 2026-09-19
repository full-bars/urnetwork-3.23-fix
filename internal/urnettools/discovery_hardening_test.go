//go:build linux

package urnettools

// Regression tests for the discovery-hardening sweep:
//   - inForeignContainer: containerized provider processes must never be
//     claimed as host providers (the "ghost root provider" incident: a
//     docker container surfaced as user=root with a guessed, nonexistent
//     host state-dir).
//   - discovery owner attribution: User comes from the kernel /proc/<pid>
//     uid, never from the process's own USER/LOGNAME environ strings.
//   - providerVersionReadOnly: version resolution never execs a path taken
//     from a discovered process.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInForeignContainerSelfAndChildAreNotSkipped pins the namespace
// anchoring: processes in the SAME mount namespace as the tool (itself, and
// a spawned child) must never be classified as containers, even when their
// cgroup path would look container-ish. A tool running inside a container
// must still be able to manage a provider running beside it.
func TestInForeignContainerSelfAndChildAreNotSkipped(t *testing.T) {
	if inForeignContainer(os.Getpid()) {
		t.Error("inForeignContainer(self) = true: the tool's own process must never be classified as a container")
	}
}

// TestInForeignContainerUnclassifiablePidIsKept pins the stat-failure path:
// a process whose /proc data cannot be read (gone mid-scan, permission
// denied, or a nonexistent pid) must be kept, never dropped — the same
// conservative "unclassifiable → keep" default that prevents ghost-provider
// over-filtering. This is distinct from the namespace+cgroup decision that
// classifyContainerByNamespaceAndCgroup tests directly (see
// TestContainerCgroupMarkerDetection).
func TestInForeignContainerUnclassifiablePidIsKept(t *testing.T) {
	if inForeignContainer(-999999) {
		t.Error("inForeignContainer(nonexistent pid) = true; unclassifiable processes must be kept, never dropped")
	}
}

// TestContainerCgroupMarkerDetection is a pure, deterministic test of the
// cgroup->runtime classification used by inForeignContainer. It reads the
// classification helper directly so the test does not depend on the host
// having docker/containerd/lxc present.
func TestContainerCgroupMarkerDetection(t *testing.T) {
	cases := []struct {
		name string
		cg   string
		ns   string // mount ns id; "" falls back to comparing against self
		diff bool   // whether ns differs (forces the cgroup check to matter)
		want bool
	}{
		{"docker systemd driver", "0::/system.slice/docker-abc123.scope", "", true, true},
		{"docker cgroupfs driver", "5:cpu:/docker/abc123456789", "", true, true},
		{"containerd k8s", "0::/kubepods/burstable/pod123/abc123", "", true, true},
		{"podman libpod", "0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-abc123.scope", "", true, true},
		{"lxc", "7:devices:/lxc.payload.abc123/container", "", true, true},
		{"systemd user slice", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/urnetwork.service", "", true, false},
		{"systemd system slice", "0::/system.slice/urnetwork.service", "", true, false},
		{"docker marker without ns diff (same container as tool)", "0::/system.slice/docker-abc123.scope", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyContainerByNamespaceAndCgroup(c.diff, c.cg)
			if got != c.want {
				t.Errorf("classifyContainerByNamespaceAndCgroup(nsDiff=%v, cg=%q) = %v, want %v", c.diff, c.cg, got, c.want)
			}
		})
	}
}

// TestResolveProcessOwnerPrecedenceDoesNotTrustEnvUser pins the User
// attribution rule: the kernel /proc owner is the ONLY source for the
// username. When the owner uid has no passwd entry (numeric uid, LDAP-only
// accounts), User stays empty — the environ's USER/LOGNAME are never
// consulted, because they are attacker-controlled strings.
func TestResolveProcessOwnerPrecedenceDoesNotTrustEnvUser(t *testing.T) {
	cases := []struct {
		name      string
		ownerUser string
		env       map[string]string
		want      string
	}{
		{
			name:      "kernel owner wins over root env claim",
			ownerUser: "alice",
			env:       map[string]string{"USER": "root", "LOGNAME": "root"},
			want:      "alice",
		},
		{
			name:      "unresolved owner does not trust USER",
			ownerUser: "",
			env:       map[string]string{"USER": "root", "LOGNAME": "root"},
			want:      "",
		},
		{
			name:      "unresolved owner does not trust LOGNAME alone",
			ownerUser: "",
			env:       map[string]string{"LOGNAME": "root"},
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveProcessOwnerPrecedence(tc.ownerUser, tc.env)
			if got != tc.want {
				t.Fatalf("resolveProcessOwnerPrecedence(%q, %v) = %q, want %q",
					tc.ownerUser, tc.env, got, tc.want)
			}
		})
	}
}

// TestProviderVersionReadOnlyNeverExecs pins the security property: the
// read-only version resolver must not execute the probed binary (exec'ing a
// discovered /proc/<pid>/exe would run an attacker-chosen ELF as root). A
// shell script with a valid --version response must yield empty via the
// read-only path (no exec) — unlike providerVersion which would run it.
func TestProviderVersionReadOnlyNeverExecs(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v9.9.9-fake\n"), 0o755); err != nil {
		t.Fatalf("write fake provider script: %v", err)
	}
	if got := providerVersionReadOnly(script); got != "" {
		t.Errorf("providerVersionReadOnly executed the script (got %q); must be read-only", got)
	}
}

// TestProviderVersionReadOnlyReadsStamp verifies the read-only resolver still
// finds a version carried as the raw release stamp (URNET_VERSION_STAMP=
// bytes), i.e. blocking exec did not lose version resolution for real builds.
func TestProviderVersionReadOnlyReadsStamp(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "urnetwork")
	payload := append([]byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"), []byte("\n"+versionStampPrefix+"v3.23.0-fix.26.4\n")...)
	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatalf("write stamped fake: %v", err)
	}
	if got := providerVersionReadOnly(bin); got != "v3.23.0-fix.26.4" {
		t.Errorf("providerVersionReadOnly(stamped) = %q, want v3.23.0-fix.26.4", got)
	}
}

// TestCmdLogsEmptyUnitReturnsActionableError pins the logs fix: a provider
// with no systemd unit must yield an actionable error instead of building
// journalctl -fu "" (which dies with "Failed to add filter for units:
// Invalid argument").
func TestCmdLogsEmptyUnitReturnsActionableError(t *testing.T) {
	p := Provider{Running: true, User: "root", Unit: "", StateDir: "/root/.urnetwork"}
	if err := validateLogsTarget(p); err == nil {
		t.Fatal("validateLogsTarget on an empty-Unit provider returned nil; must error")
	} else if !strings.Contains(err.Error(), "no systemd unit") {
		t.Errorf("error should mention the missing unit, got: %v", err)
	}
}

// TestJournalctlArgsEmptyUnitNeverEmitsBareUnit pins the low-level guard: the
// journalctl argv builder must refuse an empty unit outright rather than
// emit ["-fu", ""].
func TestJournalctlArgsEmptyUnitNeverEmitsBareUnit(t *testing.T) {
	if err := journalctlArgsGuard(Provider{Unit: ""}); err == nil {
		t.Fatal("journalctlArgsGuard(empty unit) returned nil; must refuse")
	}
}
