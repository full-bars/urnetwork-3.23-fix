package urnettools

import (
	"os"
	"strings"
	"testing"
)

// `off` means CLEAR for tuning keys: it removes the override and restores
// the provider's default. Three keys are exempt because `off` is a real
// value for them, not an absence.
//
// gogc must not join that list. The provider maps a real gogc=off to
// debug.SetGCPercent(-1), which disables garbage collection entirely and
// lets the heap grow without bound. Several fleet nodes run under a
// gigabyte. Making `set gogc off` mean that turns a command whose whole
// purpose is returning to safe defaults into an out-of-memory risk, with no
// warning and no confirmation distinguishing it from clearing any other key.
//
// Keeping `off` as clear leaves the disable capability unreachable from the
// CLI. That is a real gap, but the fix for it is a distinct, self-describing
// value, not overloading the word every other key uses for "remove this".
func TestGogcOffIsTreatedAsClearNotAsDisable(t *testing.T) {
	b, err := os.ReadFile("restore_delegate.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, `value == "off"`) {
			continue
		}
		if strings.Contains(line, `canonicalKey != "gogc"`) {
			t.Errorf("gogc is exempt from off-means-clear, so `set gogc off` disables GC:\n  %s", strings.TrimSpace(line))
		}
	}
}
