package urnettools

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// stubTopCmd swaps the terminal and screen seams and isolates HOME, returning
// counters for how often discovery ran and a screen was opened.
func stubTopCmd(t *testing.T, isTTY bool, providers []Provider, openErr error) (discovered, opened *int) {
	t.Helper()
	stubStatus(t, providers, nil, false)
	origTTY, origOpen := topStdoutIsTerminal, topOpenScreen
	t.Cleanup(func() { topStdoutIsTerminal, topOpenScreen = origTTY, origOpen })
	discovered, opened = new(int), new(int)
	origDisc := discoverStatusFn
	discoverStatusFn = func() []Provider { *discovered++; return origDisc() }
	topStdoutIsTerminal = func() bool { return isTTY }
	topOpenScreen = func() (tcellui.Screen, error) {
		*opened++
		return nil, openErr
	}
	return discovered, opened
}

// `urtop` from cron, a pipe or a redirect has no terminal to draw on. It must
// say so and point at the scriptable command, and it must do so before it
// discovers providers or touches the terminal.
func TestCmdTopRefusesWithoutATerminalBeforeDoingAnythingElse(t *testing.T) {
	discovered, opened := stubTopCmd(t, false, []Provider{{Unit: "a", Running: true}}, nil)

	err := cmdTop(nil)
	if err == nil {
		t.Fatal("top without a terminal must fail")
	}
	for _, want := range []string{"interactive terminal", "urnet-tools status", "--json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if *discovered != 0 || *opened != 0 {
		t.Fatalf("nothing else may run first: discovery=%d screenOpens=%d", *discovered, *opened)
	}
}

// A bad flag is the operator's mistake and is reported as such, even without a
// terminal, since it would fail the same way on one.
func TestCmdTopRejectsABadIntervalBeforeTheTerminalCheck(t *testing.T) {
	stubTopCmd(t, false, nil, nil)
	err := cmdTop([]string{"--interval", "10ms"})
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("a too-small --interval should be rejected, got %v", err)
	}
}

// If the terminal cannot be opened (a dumb terminal, a broken TERM), the error
// names the alternative rather than dumping a tcell error alone.
func TestCmdTopReportsAScreenThatCannotOpen(t *testing.T) {
	_, opened := stubTopCmd(t, true, []Provider{{Unit: "a", Running: true}}, errors.New("terminal too dumb"))

	err := cmdTop(nil)
	if err == nil {
		t.Fatal("an unopenable screen must fail")
	}
	if *opened != 1 {
		t.Fatalf("the screen should be tried once, got %d", *opened)
	}
	for _, want := range []string{"cannot open the terminal", "terminal too dumb", "urnet-tools status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}
