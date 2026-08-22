package urnettools

import (
	"strings"
	"testing"
)

// F1 regression: command-specific flags must reach the command's own parse
// loop (strict target parsing used to reject them as unknown first). In real
// dispatch, -f/-n/-h are stripped by parseGlobalFlags BEFORE the command
// runs, so command args here contain only the command's own flags.
// The box may or may not have providers; the invariant under test is that
// the flag is NOT rejected at the parse layer.
func TestF1_UpdateFlagsReachLoop(t *testing.T) {
	err := cmdUpdate([]string{"--tag", "v9.9.9"}, true, true)
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--tag was rejected as unknown flag: %v", err)
	}
}

func TestF1_ProxyAllReachesLoop(t *testing.T) {
	err := cmdProxy([]string{"clear", "--all"}, false, true)
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--all was rejected as unknown flag: %v", err)
	}
}

// TestF1_HubTagSurvivesParse pins the parse-layer mechanism for hub: the
// lenient target parse must preserve --tag=... so cmdHubInstall can read it.
// (Invoking the full install path would download a binary — out of scope
// for a parse test, and hermeticity matters.)
func TestF1_HubTagSurvivesParse(t *testing.T) {
	_, rest, err := parseTargetFlagsLenient([]string{"install", "--tag=v9.9.9"})
	if err != nil {
		t.Fatalf("lenient parse: %v", err)
	}
	joined := strings.Join(rest, " ")
	if !strings.Contains(joined, "--tag=") {
		t.Fatalf("--tag= must survive the lenient parse, got: %q", joined)
	}
}

func TestF1_UnknownFlagStillRejected(t *testing.T) {
	err := cmdUpdate([]string{"--netwrok"}, true, true)
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--netwrok typo must still be rejected, got: %v", err)
	}
}
