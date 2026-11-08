package urnettools

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestFreeBSDSetupPrintOnlyAllModes(t *testing.T) {
	for _, root := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			var out bytes.Buffer
			calls := 0
			run := func(string, ...string) ([]byte, error) { calls++; return nil, nil }
			cfg := &SystemPlatformConfigurer{PlatformName: "freebsd", ExecCmd: run,
				WriteFile: func(string, []byte, os.FileMode) error { calls++; return nil }}
			err := RunExtenderSetup(ExtenderSetupOptions{Platform: "freebsd", PlatformCfg: cfg,
				Stdout: &out, IsRoot: func() bool { return root }, DryRun: dry, Yes: true,
				ElevateFunc: func([]string) error { calls++; return nil },
				PortChecker: &PortChecker{Platform: "freebsd", ExecCmd: run},
				FirewallMgr: &FirewallManager{Platform: "freebsd", ExecCmd: run, LookPath: func(string) (string, error) { calls++; return "", nil }}})
			if err != nil || calls != 0 {
				t.Errorf("root=%v dry=%v: err=%v calls=%d", root, dry, err, calls)
			}
			for _, want := range []string{"untested this release", "\n  previous_reservedhigh=$(sysctl -n net.inet.ip.portrange.reservedhigh) && sysctl net.inet.ip.portrange.reservedhigh=0\n", "\n  sysctl net.inet.ip.portrange.reservedhigh=\"$previous_reservedhigh\"\n", "status requires", "ruleset-specific", "UDP 53 is optional and user-managed"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("root=%v dry=%v missing %q: %s", root, dry, want, out.String())
				}
			}
		}
	}
}

func TestFreeBSDApplyRequirementNeverMutates(t *testing.T) {
	calls := 0
	sp := &SystemPlatformConfigurer{PlatformName: "freebsd", ExecCmd: func(string, ...string) ([]byte, error) { calls++; return nil, nil }, ReadFile: func(string) ([]byte, error) { calls++; return nil, nil }, WriteFile: func(string, []byte, os.FileMode) error { calls++; return nil }}
	applied, _, _ := sp.ApplyRequirement("", "provider")
	if applied || calls != 0 {
		t.Fatalf("applied=%v calls=%d", applied, calls)
	}
}
