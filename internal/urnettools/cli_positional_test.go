package urnettools

import (
	"strings"
	"testing"
)

// status and top choose their provider with flags. A bare argument used to be
// ignored, so `status b.service` summarized every provider and hid the mistake.
func TestStatusRejectsAPositionalArgument(t *testing.T) {
	ps := []Provider{
		{Unit: "a.service", User: "ua", StateDir: t.TempDir(), Running: true},
		{Unit: "b.service", User: "ub", StateDir: t.TempDir(), Running: true},
	}
	stubStatus(t, ps, nil, true)
	var err error
	out := captureStdout(t, func() { err = cmdStatus([]string{"b.service"}) })
	if err == nil || !strings.Contains(err.Error(), "status takes no positional arguments") || !strings.Contains(err.Error(), "--unit") {
		t.Fatalf("err = %v, want a positional argument error naming the target flags", err)
	}
	if out != "" {
		t.Fatalf("nothing may be printed for a rejected command, got %q", out)
	}
}

func TestTopRejectsAPositionalArgument(t *testing.T) {
	ps := []Provider{{Unit: "a.service", User: "ua", StateDir: t.TempDir(), Running: true}}
	stubStatus(t, ps, nil, true)
	orig := topStdoutIsTerminal
	topStdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() { topStdoutIsTerminal = orig })
	err := cmdTop([]string{"a.service"})
	if err == nil || !strings.Contains(err.Error(), "top takes no positional arguments") {
		t.Fatalf("err = %v, want a positional argument error", err)
	}
}
