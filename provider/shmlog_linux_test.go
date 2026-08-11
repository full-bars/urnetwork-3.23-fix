//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

func TestRamlogsTailHintEnvOverride(t *testing.T) {
	t.Setenv("URNETWORK_CONTAINER_NAME", "urfix")
	got := ramlogsTailHint()
	want := "docker exec urfix tail -f /dev/shm/urnetwork.log"
	if got != want {
		t.Fatalf("ramlogsTailHint() with env set = %q, want %q", got, want)
	}

	// Option A display: the resolved line first, then the universal template.
	got2 := ramlogsTailHintWithTemplate()
	want2 := want + "\n[ramlogs] (any container name: docker exec <container> tail -f /dev/shm/urnetwork.log)"
	if got2 != want2 {
		t.Fatalf("ramlogsTailHintWithTemplate() = %q, want %q", got2, want2)
	}
}

func TestRamlogsTailHintBareMetal(t *testing.T) {
	// On this host /.dockerenv does not exist, so the hint must be a plain
	// tail -f (a docker hint would be wrong on bare metal).
	if _, err := os.Stat(ramlogsDockerEnvPath); err == nil {
		t.Skip("running inside a container; bare-metal path not applicable here")
	}
	t.Setenv("URNETWORK_CONTAINER_NAME", "")
	got := ramlogsTailHint()
	if strings.HasPrefix(got, "docker exec ") {
		t.Fatalf("ramlogsTailHint() on bare metal = %q, want a plain tail -f hint", got)
	}
	if !strings.HasSuffix(got, "tail -f /dev/shm/urnetwork.log") {
		t.Fatalf("ramlogsTailHint() on bare metal = %q, want a tail -f of the shm log", got)
	}
}

// TestRamlogsTailHintEnvWhitespaceOnly treats a whitespace-only env value as
// unset (falls through to the bare-metal path on this host).
func TestRamlogsTailHintEnvWhitespaceOnly(t *testing.T) {
	t.Setenv("URNETWORK_CONTAINER_NAME", "   ")
	got := ramlogsTailHint()
	if strings.HasPrefix(got, "docker exec ") {
		t.Fatalf("whitespace-only env = %q, want non-docker hint", got)
	}
	if !strings.HasSuffix(got, "tail -f /dev/shm/urnetwork.log") {
		t.Fatalf("whitespace-only env = %q, want tail -f of shm log", got)
	}
}

// TestRamlogsTailHintDockerPathSimulated exercises the container-ID fallback
// via the ramlogsDockerEnvPath seam (no root needed).
func TestRamlogsTailHintDockerPathSimulated(t *testing.T) {
	t.Setenv("URNETWORK_CONTAINER_NAME", "")
	tmp := t.TempDir()
	marker := tmp + "/dockerenv"
	if err := os.WriteFile(marker, []byte(""), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	orig := ramlogsDockerEnvPath
	ramlogsDockerEnvPath = marker
	defer func() { ramlogsDockerEnvPath = orig }()

	host, _ := os.Hostname()
	want := "docker exec " + host + " tail -f /dev/shm/urnetwork.log"
	if got := ramlogsTailHint(); got != want {
		t.Fatalf("simulated docker = %q, want %q", got, want)
	}
}

// TestRamlogsTailHintWithTemplateBareMetal: no <container> template line on
// the bare-metal path.
func TestRamlogsTailHintWithTemplateBareMetal(t *testing.T) {
	if _, err := os.Stat(ramlogsDockerEnvPath); err == nil {
		t.Skip("running inside a container; bare-metal path not applicable here")
	}
	t.Setenv("URNETWORK_CONTAINER_NAME", "")
	got := ramlogsTailHintWithTemplate()
	if strings.Contains(got, "<container>") {
		t.Fatalf("bare-metal hint = %q, must not contain the docker template", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("bare-metal hint = %q, must be a single line", got)
	}
}
