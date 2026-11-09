package urnettools

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestExtenderRouteExistingCapabilityAndSkip(t *testing.T) {
	old := setupDiscoverFn
	setupDiscoverFn = func() []Provider { return []Provider{{Binary: "/provider", User: "provider"}} }
	defer func() { setupDiscoverFn = old }()
	for _, route := range []string{"capability", "skip"} {
		t.Run(route, func(t *testing.T) {
			var out bytes.Buffer
			mutations := 0
			execFake := func(name string, args ...string) ([]byte, error) {
				switch name {
				case "getcap":
					return []byte("/provider cap_net_bind_service=ep"), nil
				case "sysctl", "setcap":
					mutations++
				}
				return nil, nil
			}
			cfg := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: execFake, ReadFile: func(string) ([]byte, error) { return []byte("1234"), nil }}
			err := RunExtenderSetup(ExtenderSetupOptions{Platform: "linux", Route: route, Yes: true, NonInteractive: true, IsRoot: func() bool { return true }, Stdout: &out, PlatformCfg: cfg, PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFake}, FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", os.ErrNotExist }, ExecCmd: execFake}})
			if err != nil || mutations != 0 {
				t.Fatalf("route=%s err=%v mutations=%d output=%s", route, err, mutations, out.String())
			}
			if route == "capability" && !strings.Contains(out.String(), "already configured") {
				t.Fatalf("capability should be idempotent: %s", out.String())
			}
			if route == "skip" && !strings.Contains(out.String(), "skipped") {
				t.Fatalf("skip not reported: %s", out.String())
			}
		})
	}
}

func TestExtenderCapabilityRemovalConsentAndOtherCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, caps                                 string
		tty, consent, nonInteractive, unprivileged bool
		wantRemoved                                bool
	}{
		{"no tty even with yes", "/provider cap_net_bind_service=ep", false, true, false, false, false},
		{"noninteractive even with tty", "/provider cap_net_bind_service=ep", true, true, true, false, false},
		{"unprivileged even with consent", "/provider cap_net_bind_service=ep", true, true, false, true, false},
		{"declined", "/provider cap_net_bind_service=ep", true, false, false, false, false},
		{"explicit consent", "/provider cap_net_bind_service=ep", true, true, false, false, true},
		{"other capability preserved", "/provider cap_net_bind_service,cap_net_admin=ep", true, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			removed, prompts := 0, 0
			cfg := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
				switch name {
				case "getcap":
					return []byte(tc.caps), nil
				case "setcap":
					if len(args) != 2 || args[0] != "-r" || args[1] != "/provider" {
						t.Fatalf("unexpected removal %v", args)
					}
					removed++
					return nil, nil
				}
				t.Fatalf("unexpected %s %v", name, args)
				return nil, nil
			}}
			err := offerExtenderCapabilityRemoval(&out, cfg, []Provider{{Binary: "/provider", User: "provider"}}, ExtenderSetupOptions{
				InteractiveFunc: func() bool { return tc.tty }, NonInteractive: tc.nonInteractive,
				IsRoot:                      func() bool { return !tc.unprivileged },
				RemoveCapabilityConfirmFunc: func() (bool, error) { prompts++; return tc.consent, nil },
			})
			if err != nil || (removed == 1) != tc.wantRemoved {
				t.Fatalf("err=%v removed=%d output=%s", err, removed, out.String())
			}
			if prompts != 0 && (!tc.tty || tc.nonInteractive || tc.unprivileged || strings.Contains(tc.caps, "cap_net_admin")) {
				t.Fatalf("unexpected prompt: %d", prompts)
			}
			if tc.wantRemoved && !strings.Contains(out.String(), "restart the provider") {
				t.Fatalf("missing restart guidance: %s", out.String())
			}
		})
	}
}

func TestExtenderCapabilityRemovalInspectionFailure(t *testing.T) {
	var out bytes.Buffer
	err := offerExtenderCapabilityRemoval(&out, &SystemPlatformConfigurer{ExecCmd: func(string, ...string) ([]byte, error) { return nil, errors.New("unavailable") }}, []Provider{{Binary: "/provider"}}, ExtenderSetupOptions{})
	if err != nil || !strings.Contains(out.String(), "cannot inspect") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}

func TestExtenderRouteSysctlWithExistingCapability(t *testing.T) {
	old := setupDiscoverFn
	setupDiscoverFn = func() []Provider { return []Provider{{Binary: "/provider", User: "provider"}} }
	defer func() { setupDiscoverFn = old }()
	live := "1024"
	wrote := false
	execFake := func(name string, args ...string) ([]byte, error) {
		switch name {
		case "getcap":
			return []byte("/provider cap_net_bind_service=ep"), nil
		case "sysctl":
			live = "443"
		}
		return nil, nil
	}
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: execFake, ReadFile: func(path string) ([]byte, error) {
		if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			return []byte(live), nil
		}
		return nil, os.ErrNotExist
	}, WriteFile: func(string, []byte, os.FileMode) error { wrote = true; return nil }}
	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{Platform: "linux", Route: "sysctl", Yes: true, NonInteractive: true, IsRoot: func() bool { return true }, Stdout: &out, PlatformCfg: cfg, PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFake}, FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", os.ErrNotExist }, ExecCmd: execFake}})
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || !strings.Contains(out.String(), "sudo setcap -r /provider") {
		t.Fatalf("expected sysctl write and capability warning; wrote=%v output=%s", wrote, out.String())
	}
}
