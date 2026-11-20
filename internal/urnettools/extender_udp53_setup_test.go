package urnettools

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestExtenderSetupUDP53FlagAndRoutes(t *testing.T) {
	old := runExtenderSetupFn
	defer func() { runExtenderSetupFn = old }()
	var got ExtenderSetupOptions
	runExtenderSetupFn = func(opts ExtenderSetupOptions) error { got = opts; return nil }
	if err := cmdExtenderSetup([]string{"setup", "--route=sysctl", "--udp53", "-y"}); err != nil {
		t.Fatal(err)
	}
	if !got.UDP53 || got.Route != "sysctl" || !got.Yes {
		t.Fatalf("unexpected options: %+v", got)
	}
	if err := cmdExtenderSetup([]string{"setup", "--route=sysctl", "--udp53", "--force"}); err != nil {
		t.Fatal(err)
	}
	if got.Yes {
		t.Fatal("--force must not bypass UDP 53 consent")
	}
	for _, route := range []string{"capability", "skip"} {
		err := RunExtenderSetup(ExtenderSetupOptions{Platform: "linux", UDP53: true, Route: route})
		if err == nil || !strings.Contains(err.Error(), "sysctl") {
			t.Fatalf("route %s: expected sysctl error, got %v", route, err)
		}
	}
	if err := RunExtenderSetup(ExtenderSetupOptions{Platform: "freebsd", UDP53: true, Route: "sysctl"}); err == nil {
		t.Fatal("FreeBSD must refuse --udp53")
	}
}

func TestExtenderUDP53ThresholdStatus(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"53", "in force (live value 53)"},
		{"0", "in force (live value 0)"},
		{"443", "not in force (live value 443)"},
		{"invalid", "unknown"},
	} {
		got := udp53ThresholdStatus("linux", func(string) ([]byte, error) { return []byte(tc.value), nil })
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%q: got %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestExtenderSetupUDP53PlanAndConsent(t *testing.T) {
	old := setupDiscoverFn
	setupDiscoverFn = func() []Provider { return []Provider{{User: "alice", Binary: "/provider"}} }
	defer func() { setupDiscoverFn = old }()
	for _, tc := range []struct {
		name              string
		yes, dry, consent bool
		wantWrite         bool
	}{
		{"dry run", false, true, false, false},
		{"declined", false, false, false, false},
		{"non interactive", false, false, false, false},
		{"confirmed", false, false, true, true},
		{"yes", true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			live := "443\n"
			writes := 0
			var content string
			execFn := func(name string, args ...string) ([]byte, error) {
				switch name {
				case "ss":
					return []byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"), nil
				case "sysctl":
					live = "53\n"
				case "sudo":
					return []byte("extender-bind-probe: proven\n"), nil
				}
				return nil, nil
			}
			cfg := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: execFn,
				ReadFile: func(path string) ([]byte, error) {
					if path == extenderSysctlPortStartPath {
						return []byte(live), nil
					}
					return nil, os.ErrNotExist
				},
				WriteFile:  func(_ string, data []byte, _ os.FileMode) error { writes++; content = string(data); return nil },
				LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			}
			confirm := func() (bool, error) { return tc.consent, nil }
			if tc.name == "non interactive" {
				confirm = nil
			}
			err := RunExtenderSetup(ExtenderSetupOptions{Platform: "linux", UDP53: true, Route: "sysctl", Yes: tc.yes, DryRun: tc.dry, NonInteractive: tc.name == "non interactive",
				IsRoot: func() bool { return true }, Stdout: &out, Stderr: &out,
				ConfirmFunc: confirm, PlatformCfg: cfg,
				PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFn},
				FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", os.ErrNotExist }, ExecCmd: execFn},
			})
			if tc.name == "declined" || tc.name == "non interactive" {
				if err == nil {
					t.Fatal("expected confirmation error")
				}
			} else if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if (writes > 0) != tc.wantWrite {
				t.Fatalf("writes=%d, output=%s", writes, out.String())
			}
			if tc.wantWrite && content != "net.ipv4.ip_unprivileged_port_start=53\n" {
				t.Fatalf("content=%q", content)
			}
			for _, text := range []string{"changes from 443 to 53", "Any local user can bind ports 53 to 1023", "Recovery command:", "ip_unprivileged_port_start=443"} {
				if !strings.Contains(out.String(), text) {
					t.Errorf("missing %q in %s", text, out.String())
				}
			}
		})
	}
}
