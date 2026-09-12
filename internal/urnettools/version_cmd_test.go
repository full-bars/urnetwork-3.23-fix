package urnettools

import (
	"strings"
	"testing"
)

// `version` and `-v` answer different questions on purpose: `-v` reports what
// this binary is, `version` reports what is actually running on the box, and
// the upgrade instructions tell operators to verify with `version` for exactly
// that reason.
//
// There were two implementations of `version`. The one in cli.go prints the
// provider inventory; the Cobra command printed only the bare tool version and
// was unreachable, because the cli.go switch returns first. Unreachable code
// that disagrees about what the command does is a trap: remove the intercept
// and `version` silently stops answering the question the docs rely on.
func TestVersionCommandReportsProviderInventory(t *testing.T) {
	var cobraOut string
	for _, c := range buildRootCmd().Commands() {
		if c.Name() != "version" {
			continue
		}
		cobraOut = captureStdout(t, func() {
			if err := c.RunE(c, nil); err != nil {
				t.Fatalf("version command: %v", err)
			}
		})
	}
	if cobraOut == "" {
		t.Fatal("no version command registered on the root")
	}

	// The inventory form leads with "urnet-tools <version>". The bare form
	// prints the version alone, which is what -v is for.
	if !strings.HasPrefix(cobraOut, "urnet-tools ") {
		t.Errorf("version printed %q, want the inventory form starting \"urnet-tools \"; "+
			"the Cobra command must not be a second, divergent implementation", strings.TrimSpace(cobraOut))
	}
}

// -v stays the bare form. It answers "what is this binary" and must not pay
// for provider discovery.
func TestDashVPrintsBareToolVersion(t *testing.T) {
	out := captureStdout(t, func() {
		if err := Run([]string{"-v"}); err != nil {
			t.Fatalf("-v: %v", err)
		}
	})
	if strings.TrimSpace(out) != ToolVersion {
		t.Errorf("-v printed %q, want the bare version %q", strings.TrimSpace(out), ToolVersion)
	}
}
