package urnettools

import (
	"os"
	"strings"
	"testing"
)

// stubStdin fixes what unattendedUpdate sees for one test. `go test` binds
// the test binary's own stdin to /dev/null, so without an override for the
// null-device check a test cannot represent the pipe case at all.
func stubStdin(t *testing.T, interactive, devNull bool) {
	t.Helper()
	oldI, oldN := stdinIsInteractiveOverride, stdinIsDevNullOverride
	stdinIsInteractiveOverride = func() bool { return interactive }
	stdinIsDevNullOverride = func() bool { return devNull }
	t.Cleanup(func() {
		stdinIsInteractiveOverride, stdinIsDevNullOverride = oldI, oldN
	})
}

// unattendedUpdate decides whether an update run may skip the confirmation
// prompts. The weekly urnetwork-update.timer is why it exists: its ExecStart
// is a bare `urnet-tools update` with no -y and systemd hands it /dev/null on
// stdin, so gating the prompt on !force alone made the unit exit 1 on every
// run and the fleet's auto-update was silently dead.
func TestUnattendedUpdate(t *testing.T) {
	cases := []struct {
		name         string
		force        bool
		interactive  bool
		devNull      bool
		invocationID string
		want         bool
	}{
		{"operator at a terminal", false, true, false, "", false},
		{"systemd timer", false, false, true, "test-invocation", true},
		{"systemd without INVOCATION_ID, still /dev/null", false, false, true, "", true},
		{"cron: /dev/null, no INVOCATION_ID", false, false, true, "", true},
		{"ssh without a pty", false, false, false, "", false},
		{"piped stdin", false, false, false, "", false},
		{"-f at a terminal", true, true, false, "", true},
		{"-f piped", true, false, false, "", true},
		{"INVOCATION_ID wins over a non-null stdin", false, false, false, "test-invocation", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubStdin(t, tc.interactive, tc.devNull)
			t.Setenv("INVOCATION_ID", tc.invocationID)
			if got := unattendedUpdate(tc.force); got != tc.want {
				t.Fatalf("unattendedUpdate(force=%v) interactive=%v devNull=%v INVOCATION_ID=%q = %v, want %v",
					tc.force, tc.interactive, tc.devNull, tc.invocationID, got, tc.want)
			}
		})
	}
}

// The null-device check must actually fire on a real /dev/null. This is the
// test that catches an encoding mistake in the detection itself: a previous
// attempt compared the device number against 0x0300, but Linux makedev(1,3)
// is 0x103, so it silently returned false and the whole path was dead code.
// `go test` hands the test binary /dev/null on stdin, so the real thing is
// available here with no setup.
func TestStdinIsDevNullDetectsTheRealNullDevice(t *testing.T) {
	if stdinIsDevNullOverride != nil {
		t.Fatal("override leaked from another test")
	}
	self, err := os.Stdin.Stat()
	if err != nil {
		t.Skipf("cannot stat stdin: %v", err)
	}
	null, err := os.Stat(os.DevNull)
	if err != nil {
		t.Skipf("cannot stat %s: %v", os.DevNull, err)
	}
	if !os.SameFile(self, null) {
		t.Skip("this runner does not give the test binary /dev/null on stdin")
	}
	if !stdinIsDevNull() {
		t.Fatal("stdinIsDevNull() = false for a stdin that IS /dev/null; the systemd and cron path is dead code")
	}
}

// A systemd run must proceed without touching stdin.
func TestConfirmGateMultiProceedsUnderSystemd(t *testing.T) {
	stubStdin(t, false, true)
	t.Setenv("INVOCATION_ID", "test-invocation")

	targets := []Provider{{Unit: "urnetwork.service", User: "user", StateDir: "/home/user/.urnetwork"}}
	ok, err := confirmGateMulti("update 1 provider(s) to v3.23.0-fix.31.7", targets, unattendedUpdate(false), false)
	if err != nil {
		t.Fatalf("confirmGateMulti errored on a systemd run: %v", err)
	}
	if !ok {
		t.Fatal("confirmGateMulti refused a systemd run; the update timer can never proceed")
	}
}

// The safety half, and the reason the gate is not simply "no terminal".
// `ssh node 'urnet-tools update'` gets no pty, and on a single-provider box
// a target resolves without an explicit flag, so treating that as consent
// would update and restart production off an accidental invocation.
func TestSshWithoutPtyStillRefuses(t *testing.T) {
	stubStdin(t, false, false)
	t.Setenv("INVOCATION_ID", "")

	if unattendedUpdate(false) {
		t.Fatal("a non-terminal, non-null stdin was treated as unattended; ssh/Ansible/nohup would update without confirmation")
	}

	targets := []Provider{{Unit: "urnetwork.service", User: "user", StateDir: "/home/user/.urnetwork"}}
	ok, err := confirmGateMulti("update 1 provider(s) to v3.23.0-fix.31.7", targets, unattendedUpdate(false), false)
	if ok {
		t.Fatal("confirmGateMulti proceeded for an ssh run with no pty")
	}
	if err == nil {
		t.Fatal("expected a loud refusal, got a silent decline")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("refusal should name the cause so an operator knows to pass -y; got %v", err)
	}
}

// A piped answer is NOT read. confirmStdinRead refuses before reading
// whenever stdin is not a terminal, and it is shared with the hub, session
// and legacy destructive commands which must keep refusing. This pins the
// documented behaviour so the comment and the code cannot drift apart.
func TestPipedAnswerIsNotRead(t *testing.T) {
	stubStdin(t, false, false)
	t.Setenv("INVOCATION_ID", "")

	if _, err := confirmStdinRead("Update to this version? [Y/n]: "); err == nil {
		t.Fatal("confirmStdinRead accepted a piped answer; the docs on unattendedUpdate say it does not")
	}
}

// A dry run must never act, whoever invoked it.
func TestDryRunDoesNotActEvenUnderSystemd(t *testing.T) {
	stubStdin(t, false, true)
	t.Setenv("INVOCATION_ID", "test-invocation")

	targets := []Provider{{Unit: "urnetwork.service", User: "user", StateDir: "/home/user/.urnetwork"}}
	ok, err := confirmGateMulti("update 1 provider(s) to v3.23.0-fix.31.7", targets, unattendedUpdate(false), true)
	if err != nil {
		t.Fatalf("dry run errored: %v", err)
	}
	if ok {
		t.Fatal("dry run returned go-ahead; the caller would act")
	}
}

// The pickers keep their own gate. A systemd run has no terminal, so it must
// not be offered a numbered picker even though it may skip the prompts.
// Tying the two together would offer a picker to a run that cannot answer.
func TestPickerGateStaysSeparateFromPromptGate(t *testing.T) {
	stubStdin(t, false, true)
	t.Setenv("INVOCATION_ID", "test-invocation")

	if forceInteractive(false) {
		t.Fatal("pickers enabled for a systemd run with no terminal")
	}
	if !unattendedUpdate(false) {
		t.Fatal("prompts not skipped for a systemd run")
	}
}
