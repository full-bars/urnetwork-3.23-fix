package urnettools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusLiveFlagInvokesLiveMode(t *testing.T) {
	origLive := runStatusLiveFn
	defer func() { runStatusLiveFn = origLive }()

	var calledProvider Provider
	var calledInterval time.Duration
	var calledPlain bool
	called := false

	runStatusLiveFn = func(p Provider, interval time.Duration, plain bool) error {
		called = true
		calledProvider = p
		calledInterval = interval
		calledPlain = plain
		return nil
	}

	p := Provider{Unit: "main.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"main.service": fixtureRaw(t, "node_snapshot_v1.json")}, true)

	// Test --live default interval (2s)
	if err := cmdStatus([]string{"--live"}); err != nil {
		t.Fatalf("cmdStatus(--live) err: %v", err)
	}
	if !called {
		t.Fatal("runStatusLiveFn was not called for --live")
	}
	if calledInterval != 2*time.Second {
		t.Fatalf("calledInterval = %v, want 2s", calledInterval)
	}
	if calledProvider.Unit != "main.service" {
		t.Fatalf("calledProvider.Unit = %q, want %q", calledProvider.Unit, "main.service")
	}
	if calledPlain {
		t.Fatal("calledPlain = true, want false")
	}

	// Test -l with --interval 5s and --plain
	called = false
	if err := cmdStatus([]string{"-l", "--interval", "5s", "--plain"}); err != nil {
		t.Fatalf("cmdStatus(-l --interval 5s --plain) err: %v", err)
	}
	if !called {
		t.Fatal("runStatusLiveFn was not called for -l")
	}
	if calledInterval != 5*time.Second {
		t.Fatalf("calledInterval = %v, want 5s", calledInterval)
	}
	if !calledPlain {
		t.Fatal("calledPlain = false, want true")
	}

	// Test -live with --interval=3s
	called = false
	if err := cmdStatus([]string{"-live", "--interval=3s"}); err != nil {
		t.Fatalf("cmdStatus(-live --interval=3s) err: %v", err)
	}
	if !called {
		t.Fatal("runStatusLiveFn was not called for -live")
	}
	if calledInterval != 3*time.Second {
		t.Fatalf("calledInterval = %v, want 3s", calledInterval)
	}
}

func TestStatusLiveIntervalErrors(t *testing.T) {
	p := Provider{Unit: "main.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"main.service": fixtureRaw(t, "node_snapshot_v1.json")}, true)

	// Missing argument
	err := cmdStatus([]string{"--live", "--interval"})
	if err == nil || !strings.Contains(err.Error(), "requires a duration argument") {
		t.Fatalf("expected requires duration argument, got %v", err)
	}

	// Invalid duration
	err = cmdStatus([]string{"--live", "--interval", "abc"})
	if err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("expected not a duration, got %v", err)
	}

	// Too small (< 100ms)
	err = cmdStatus([]string{"--live", "--interval", "50ms"})
	if err == nil || !strings.Contains(err.Error(), "at least 100ms") {
		t.Fatalf("expected at least 100ms error, got %v", err)
	}
}

func TestStatusRedrawLivePlain(t *testing.T) {
	out := captureStdout(t, func() {
		redrawLive("test frame\n", true)
	})
	if !strings.Contains(out, "test frame") {
		t.Fatalf("redrawLive missing text: %q", out)
	}
	if strings.Contains(out, "\x1b[H") {
		t.Fatalf("redrawLive in plain mode must not contain ANSI cursor home, got %q", out)
	}
}
