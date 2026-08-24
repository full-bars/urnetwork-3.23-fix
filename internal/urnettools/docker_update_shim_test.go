//go:build !windows

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerUpdateDispatchViaShim drives cmdDockerUpdate/`update`/`self-update`
// through a recording `docker` shim (URNET_DOCKER_BIN seam) instead of a real
// daemon. Covers the paths that regressed and were previously untested (Opus
// MEDIUM #453): the self-update alias must stay host-only, the in-container
// update must exec the right argv, dry-run must not exec, and a bare non-flag
// arg must fall through to the host self-update.
func TestDockerUpdateDispatchViaShim(t *testing.T) {
	log := filepath.Join(t.TempDir(), "docker.log")
	shim := filepath.Join(t.TempDir(), "docker")
	jwt := "header.eyJORVRXT1JLX05BTUUiOiJ0YWNvZ29uemFsZXozMDAwIiwibmV0d29ya19pZCI6IjAxOWMzYzBjLTQzNmMtNmI4Yi02OGEzLTJkMjFkZDQ4YTUwYyIsImV4cCI6MTc4ODM0NDc2Mn0.sig"
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ps\" ]; then echo 'abc123|urnet-test|urnetwork:latest|running'; exit 0; fi\n" +
		"if [ \"$1\" = \"exec\" ] && [ \"$2\" = \"abc123\" ] && [ \"$3\" = \"cat\" ]; then echo '" + jwt + "'; exit 0; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNET_DOCKER_BIN", shim)
	t.Setenv("DOCKER_SHIM_LOG", log)

	readLog := func() string { b, _ := os.ReadFile(log); return string(b) }
	resetLog := func() { _ = os.WriteFile(log, nil, 0o644) }

	if err := RunDocker([]string{"update", "--unit", "urnet-test", "-f"}); err != nil {
		t.Fatalf("update --unit -f: %v", err)
	}
	if s := readLog(); !strings.Contains(s, "exec urnet-test urnet-tools update") {
		t.Fatalf("update --unit did not exec in-container urnet-tools update; shim got %q", s)
	}
	// The host-side repair must run BEFORE the update exec, and BOTH repairs
	// (mktemp X template + pkill comm-truncation) must be applied, so in-place
	// update works even on old images with a broken update script.
	log1 := readLog()
	execIdx := strings.Index(log1, "exec urnet-test urnet-tools update")
	sed1Idx := strings.Index(log1, "XXXXXX")
	sed2Idx := strings.Index(log1, "pkill -f")
	if sed1Idx < 0 || sed2Idx < 0 {
		t.Fatalf("update did not run BOTH repair seds (mktemp + pkill); shim got %q", log1)
	}
	if execIdx >= 0 && (sed1Idx > execIdx || sed2Idx > execIdx) {
		t.Fatalf("repair sed ran after the update exec; shim got %q", log1)
	}

	resetLog()
	err := RunDocker([]string{"self-update", "--unit", "urnet-test"})
	if s := readLog(); strings.Contains(s, "exec urnet-test") {
		t.Fatalf("self-update routed to container exec; shim got %q", s)
	}
	if err == nil || !strings.Contains(err.Error(), "self-update") {
		t.Fatalf("self-update --unit = %v, want a host self-update error", err)
	}

	resetLog()
	if err := RunDocker([]string{"update", "--unit", "urnet-test", "-n"}); err != nil {
		t.Fatalf("update --unit -n: %v", err)
	}
	if s := readLog(); strings.Contains(s, "exec urnet-test") {
		t.Fatalf("dry-run performed an exec; shim got %q", s)
	}

	resetLog()
	_ = RunDocker([]string{"update", "nonexistent-container"})
	if s := readLog(); strings.Contains(s, "exec urnet-test") {
		t.Fatalf("bare update arg routed to container exec; shim got %q", s)
	}
}
