//go:build linux

package main

import (
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
}

func TestRamlogsTailHintBareMetal(t *testing.T) {
	// On this host /.dockerenv does not exist, so the hint must be a plain
	// tail -f (a docker hint would be wrong on bare metal).
	t.Setenv("URNETWORK_CONTAINER_NAME", "")
	got := ramlogsTailHint()
	if strings.HasPrefix(got, "docker exec ") {
		t.Fatalf("ramlogsTailHint() on bare metal = %q, want a plain tail -f hint", got)
	}
	if !strings.HasSuffix(got, "tail -f /dev/shm/urnetwork.log") {
		t.Fatalf("ramlogsTailHint() on bare metal = %q, want a tail -f of the shm log", got)
	}
}
