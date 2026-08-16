//go:build linux

package urnettools

import (
	"bufio"
	"strings"
	"testing"
)

// TestSelectTargetInteractive_NoCoverageInPR exercises selectTargetInteractive,
// which PR #352 added with zero direct test coverage (only indirectly reached
// through cmdDockerStatus/cmdDockerExec/cmdDockerLogs, none of which are
// exercised in the existing test suite either). It pins the three branches
// documented in the function's own comment: explicit target resolves
// strictly, sole provider auto-selects without prompting, and multiple
// providers with no target pop the interactive picker.
func TestSelectTargetInteractive_NoCoverageInPR(t *testing.T) {
	forceInteractiveForTest(t)
	p1 := Provider{Unit: "urnetwork-1.service", User: "u1", Network: "net1", StateDir: "/s1"}
	p2 := Provider{Unit: "urnetwork-2.service", User: "u2", Network: "net2", StateDir: "/s2"}

	t.Run("zero providers errors", func(t *testing.T) {
		_, err := selectTargetInteractive(nil, Target{})
		if err == nil {
			t.Fatal("want error with zero providers, got nil")
		}
	})

	t.Run("sole provider auto-selects without touching stdin", func(t *testing.T) {
		orig := stdinReader
		defer func() { stdinReader = orig }()
		// If selectTargetInteractive prompted here, ReadString would block
		// forever on an empty reader and the test would hang/fail on read.
		stdinReader = bufio.NewReader(strings.NewReader(""))

		got, err := selectTargetInteractive([]Provider{p1}, Target{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Unit != p1.Unit {
			t.Fatalf("got %+v, want sole provider %+v", got, p1)
		}
	})

	t.Run("explicit target resolves strictly even with multiple providers", func(t *testing.T) {
		orig := stdinReader
		defer func() { stdinReader = orig }()
		stdinReader = bufio.NewReader(strings.NewReader("")) // must not be read

		got, err := selectTargetInteractive([]Provider{p1, p2}, Target{Unit: p2.Unit})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Unit != p2.Unit {
			t.Fatalf("got %+v, want explicit target %+v", got, p2)
		}
	})

	t.Run("explicit target that matches nothing still errors (no silent fallback)", func(t *testing.T) {
		_, err := selectTargetInteractive([]Provider{p1, p2}, Target{Unit: "does-not-exist"})
		if err == nil {
			t.Fatal("want error for non-matching explicit target, got nil")
		}
	})

	t.Run("multiple providers no target pops interactive picker, single pick", func(t *testing.T) {
		orig := stdinReader
		defer func() { stdinReader = orig }()
		stdinReader = bufio.NewReader(strings.NewReader("2\n"))

		got, err := selectTargetInteractive([]Provider{p1, p2}, Target{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Unit != p2.Unit {
			t.Fatalf("got %+v, want picked provider %+v", got, p2)
		}
	})

	t.Run("multiple providers no target, picker selects >1 errors instead of picking first", func(t *testing.T) {
		orig := stdinReader
		defer func() { stdinReader = orig }()
		stdinReader = bufio.NewReader(strings.NewReader("all\n"))

		_, err := selectTargetInteractive([]Provider{p1, p2}, Target{})
		if err == nil {
			t.Fatal("want error when picker resolves more than one provider, got nil")
		}
	})
}

// testErrAmbiguous is a stand-in error, distinct from selectTarget's actual
// "no providers found" case, used to show errWithDockerHint's pass-through
// path leaves any error's text untouched when there are no docker
// containers (the common CI case, since DiscoverDocker needs the docker
// binary).
type testErrAmbiguous struct{ msg string }

func (e *testErrAmbiguous) Error() string { return e.msg }

func TestErrWithDockerHint_PassThroughWhenNoDocker(t *testing.T) {
	ambiguous := &testErrAmbiguous{msg: `target unit "x" is ambiguous (2 matches); use a more specific target`}
	got := errWithDockerHint(ambiguous, 0)
	if got.Error() != ambiguous.msg {
		t.Fatalf("errWithDockerHint with no docker containers should pass the error through unchanged, got %q", got.Error())
	}
}

// TestErrWithDockerHint_SystemdPresentSuppressesHint pins the round-1 gate:
// when systemd providers exist, the docker hint must be suppressed even if
// docker containers are present — the error is a target problem, not a
// wrong-tool problem.
func TestErrWithDockerHint_SystemdPresentSuppressesHint(t *testing.T) {
	orig := discoverDockerFn
	defer func() { discoverDockerFn = orig }()
	discoverDockerFn = func() []Provider {
		return []Provider{{Unit: "urfix"}}
	}

	msg := `target unit "urnetwork.service" matches no running provider`
	got := errWithDockerHint(&testErrAmbiguous{msg: msg}, 1)
	if got.Error() != msg {
		t.Fatalf("hint must be suppressed when systemd providers exist, got %q", got.Error())
	}
	if strings.Contains(got.Error(), "urnet-docker") {
		t.Fatalf("no docker hint expected when systemd providers exist, got %q", got.Error())
	}
}

// TestErrWithDockerHint_NoSystemdDockerPresentHints pins the positive branch:
// zero systemd providers + docker containers present -> the hint fires.
func TestErrWithDockerHint_NoSystemdDockerPresentHints(t *testing.T) {
	orig := discoverDockerFn
	defer func() { discoverDockerFn = orig }()
	discoverDockerFn = func() []Provider {
		return []Provider{{Unit: "urfix"}}
	}

	got := errWithDockerHint(&testErrAmbiguous{msg: "no providers found on this box"}, 0)
	if !strings.Contains(got.Error(), "urnet-docker") {
		t.Fatalf("expected a docker hint when no systemd providers exist, got %q", got.Error())
	}
}
