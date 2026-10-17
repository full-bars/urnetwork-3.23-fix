package urnettools

// Regression tests for the unknown-flag enforcement pass (SD8 item 2):
// commands that used to ignore an unknown --flag and then act must reject it
// with the same style of error update uses. These call the parse paths only;
// every rejection happens before any provider discovery or host access.

import (
	"strings"
	"testing"
)

func TestUpdateRejectsUnknownFlag(t *testing.T) {
	err := cmdUpdate([]string{"--netwrok"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("update accepted an unknown flag: %v", err)
	}
}

func TestDefaultSetRejectsUnknownFlag(t *testing.T) {
	err := cmdDefault([]string{"set", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("default set accepted an unknown argument: %v", err)
	}
}
