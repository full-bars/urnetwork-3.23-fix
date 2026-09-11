package urnettools

import "testing"

// The weekly urnetwork-update.timer runs `urnet-tools update` with no -y and
// no TTY (systemd gives a oneshot /dev/null on stdin). Before the fix the
// version-choice prompt was gated only on !force, so confirmStdinRead
// refused the non-terminal read and the unit exited 1 on every run — the
// fleet's auto-update had been silently dead. unattendedUpdate is the single
// source of truth for "nobody is here to answer a prompt".
func TestUnattendedUpdateDetectsNonTerminalStdin(t *testing.T) {
	cases := []struct {
		name        string
		force       bool
		interactive bool
		want        bool
	}{
		{"terminal without -f prompts", false, true, false},
		{"systemd timer: no terminal, no -f", false, false, true},
		{"pipe without -f: should prompt", false, false, false},
		{"-f on a terminal", true, true, true},
		{"-f without a terminal", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := stdinIsInteractiveOverride
			stdinIsInteractiveOverride = func() bool { return tc.interactive }
			t.Cleanup(func() { stdinIsInteractiveOverride = old })
			// Systemd timer tests need INVOCATION_ID set to trigger the /dev/null path
			if tc.want && !tc.force {
				t.Setenv("INVOCATION_ID", "test-invocation")
			}

			if got := unattendedUpdate(tc.force); got != tc.want {
				t.Fatalf("unattendedUpdate(force=%v, interactive=%v) = %v, want %v",
					tc.force, tc.interactive, got, tc.want)
			}
		})
	}
}

// Verify that force always wins, interactive always loses, and the
// systemd/INVOCATION_ID path works correctly.
func TestUnattendedUpdateForceAlwaysWins(t *testing.T) {
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return true }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
	if !unattendedUpdate(true) {
		t.Fatal("force=true should always be unattended")
	}
}

func TestUnattendedUpdateInteractiveAlwaysPrompts(t *testing.T) {
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return true }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
	if unattendedUpdate(false) {
		t.Fatal("interactive terminal should never be unattended without force")
	}
}

func TestUnattendedUpdateSystemdTimerBypassesPrompt(t *testing.T) {
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return false }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
	t.Setenv("INVOCATION_ID", "test-invocation-id")
	if !unattendedUpdate(false) {
		t.Fatal("systemd timer (INVOCATION_ID set, non-interactive) should be unattended")
	}
}

// An unattended run must still get the audit listing and must proceed without
// reading stdin. confirmGateMulti prints its "about to touch" listing to
// stderr unconditionally, so suppressing the prompt does not cost the audit
// trail that unattended runs depend on.
func TestConfirmGateMultiProceedsUnattendedWithoutReadingStdin(t *testing.T) {
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return false }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
	t.Setenv("INVOCATION_ID", "test-invocation")

	targets := []Provider{{Unit: "urnetwork.service", User: "user", StateDir: "/home/user/.urnetwork"}}
	ok, err := confirmGateMulti("update 1 provider(s) to v3.23.0-fix.31.7", targets, unattendedUpdate(false), false)
	if err != nil {
		t.Fatalf("confirmGateMulti returned error for an unattended run: %v", err)
	}
	if !ok {
		t.Fatal("confirmGateMulti refused an unattended run; the update timer can never proceed")
	}
}
