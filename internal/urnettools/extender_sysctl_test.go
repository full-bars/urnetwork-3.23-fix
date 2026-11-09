package urnettools

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderSysctlRecoveryUnknownAndApplyFailure(t *testing.T) {
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist }, WriteFile: func(string, []byte, os.FileMode) error { return nil }, ExecCmd: func(string, ...string) ([]byte, error) { return []byte("denied"), os.ErrPermission }}
	_, _, err := cfg.ApplyRequirement("/provider", "provider")
	if err == nil || !strings.Contains(err.Error(), "sysctl -w net.ipv4.ip_unprivileged_port_start=1024") || !strings.Contains(err.Error(), "prior value unknown") {
		t.Fatalf("missing safe recovery on apply failure: %v", err)
	}
}

func TestExtenderSysctlFallback(t *testing.T) {
	for _, value := range []string{"1024", "invalid", "-1"} {
		t.Run(value, func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{
				PlatformName: "linux",
				ReadFile:     func(string) ([]byte, error) { return []byte(value), nil },
				ExecCmd:      func(string, ...string) ([]byte, error) { return []byte("/provider cap_net_bind_service=ep"), nil },
			}
			ok, desc, err := cfg.CheckRequirement("/provider", "provider")
			if err != nil || !ok || !strings.Contains(desc, "cap_net_bind_service") {
				t.Fatalf("got ok=%v desc=%q err=%v", ok, desc, err)
			}
		})
	}
}

func TestExtenderSysctlExisting(t *testing.T) {
	for _, tc := range []struct {
		val      string
		wantDesc string
	}{
		{val: "443\n", wantDesc: "without a file capability"},
		{val: "0\n", wantDesc: "already lower than 443"},
		{val: "80\n", wantDesc: "already lower than 443"},
	} {
		t.Run(strings.TrimSpace(tc.val), func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{
				PlatformName: "linux",
				ReadFile: func(path string) ([]byte, error) {
					if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
						return []byte(tc.val), nil
					}
					return nil, fmt.Errorf("unexpected path %s", path)
				},
				ExecCmd: func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("no capability") },
			}
			ok, desc, err := cfg.CheckRequirement("/provider", "provider")
			if err != nil || !ok || !strings.Contains(desc, tc.wantDesc) {
				t.Fatalf("got ok=%v desc=%q err=%v, want %q", ok, desc, err, tc.wantDesc)
			}
		})
	}
}

func TestExtenderSysctlPlanAndApply(t *testing.T) {
	var writtenPath string
	var writtenData []byte
	var execCalls []string
	liveVal := "1234\n"

	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux",
		Route:        "sysctl",
		WriteFile: func(name string, data []byte, perm os.FileMode) error {
			writtenPath = name
			writtenData = data
			return nil
		},
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			execCalls = append(execCalls, setupCommand{name: name, args: args}.String())
			if name == "sysctl" {
				liveVal = "443\n"
			}
			return []byte("done\n"), nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte(liveVal), nil
			}
			return nil, os.ErrNotExist
		},
	}

	// 1. PlanRequirement
	cmds, err := cfg.PlanRequirement("/usr/local/bin/urnetwork", "provider")
	if err != nil {
		t.Fatalf("PlanRequirement: %v", err)
	}
	var cmdStrings []string
	for _, c := range cmds {
		cmdStrings = append(cmdStrings, c.String())
	}
	wantCmds := []string{
		"sh -c 'echo net.ipv4.ip_unprivileged_port_start=443 > /etc/sysctl.d/99-urnetwork-extender.conf'",
		"sysctl --system",
	}
	if !reflect.DeepEqual(cmdStrings, wantCmds) {
		t.Fatalf("planned commands %v, want %v", cmdStrings, wantCmds)
	}

	// 2. ApplyRequirement
	applied, desc, err := cfg.ApplyRequirement("/usr/local/bin/urnetwork", "provider")
	if err != nil || !applied {
		t.Fatalf("ApplyRequirement: applied=%v err=%v", applied, err)
	}
	if writtenPath != "/etc/sysctl.d/99-urnetwork-extender.conf" {
		t.Fatalf("written path %q, want /etc/sysctl.d/99-urnetwork-extender.conf", writtenPath)
	}
	if !strings.Contains(string(writtenData), "net.ipv4.ip_unprivileged_port_start=443") {
		t.Fatalf("written data %q does not contain setting", string(writtenData))
	}
	if !strings.Contains(desc, "sudo rm /etc/sysctl.d/99-urnetwork-extender.conf && sudo sysctl -w net.ipv4.ip_unprivileged_port_start=1234") {
		t.Fatalf("desc missing undo recovery command: %s", desc)
	}
}

func TestExtenderSetupSysctlRouteSelection(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	for _, route := range []string{"sysctl", "capability", "skip"} {
		t.Run(route, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var mutating []string
			var writtenFile string
			liveVal := "1234\n"
			capSet := false

			execFake := func(name string, args ...string) ([]byte, error) {
				cmdStr := setupCommand{name: name, args: args}.String()
				switch name {
				case "ss":
					return []byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"), nil
				case "getcap":
					if capSet {
						return []byte(bin + " cap_net_bind_service=ep\n"), nil
					}
					return []byte(""), nil
				case "setcap":
					mutating = append(mutating, cmdStr)
					capSet = true
					return nil, nil
				case "sysctl":
					mutating = append(mutating, cmdStr)
					liveVal = "443\n"
					return nil, nil
				case "sh":
					mutating = append(mutating, cmdStr)
					return nil, nil
				case "sudo":
					return []byte("extender-bind-probe: proven\n"), nil
				default:
					return nil, nil
				}
			}

			cfg := &SystemPlatformConfigurer{
				PlatformName: "linux",
				ExecCmd:      execFake,
				ReadFile: func(path string) ([]byte, error) {
					if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
						return []byte(liveVal), nil
					}
					return []byte("CapBnd:\t000001ffffffffff\n"), nil
				},
				WriteFile: func(name string, data []byte, perm os.FileMode) error {
					writtenFile = name
					return nil
				},
				LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
				Lstat:      os.Lstat,
				IsRoot:     func() bool { return true },
			}

			err := RunExtenderSetup(ExtenderSetupOptions{
				Platform:    "linux",
				Yes:         true,
				IsRoot:      func() bool { return true },
				Stdout:      &out,
				Stderr:      &errOut,
				Route:       route,
				PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFake},
				PlatformCfg: cfg,
				FirewallMgr: &FirewallManager{
					Platform: "linux",
					LookPath: func(string) (string, error) { return "", os.ErrNotExist },
					ExecCmd:  execFake,
				},
			})
			if err != nil {
				t.Fatalf("RunExtenderSetup failed: %v", err)
			}

			switch route {
			case "sysctl":
				if writtenFile != "/etc/sysctl.d/99-urnetwork-extender.conf" {
					t.Fatalf("expected sysctl dropin write, got %q", writtenFile)
				}
				if !strings.Contains(out.String(), "Recovery command: sudo rm /etc/sysctl.d/99-urnetwork-extender.conf && sudo sysctl -w net.ipv4.ip_unprivileged_port_start=1234") || strings.Contains(out.String(), "Recovery command: sudo rm /etc/sysctl.d/99-urnetwork-extender.conf && sudo sysctl --system") {
					t.Fatalf("output missing recovery undo command: %s", out.String())
				}
			case "capability":
				foundSetcap := false
				for _, c := range mutating {
					if strings.Contains(c, "setcap") {
						foundSetcap = true
					}
				}
				if !foundSetcap {
					t.Fatalf("expected setcap in mutating commands, got %v", mutating)
				}
			case "skip":
				if len(mutating) != 0 {
					t.Fatalf("expected zero mutating commands for skip, got %v", mutating)
				}
				if !strings.Contains(out.String(), "skipped") {
					t.Fatalf("expected skip notice in output, got: %s", out.String())
				}
			}
		})
	}
}

func TestExtenderSetupSysctlTTYRequired(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	execFake := func(name string, args ...string) ([]byte, error) {
		switch name {
		case "ss":
			return []byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"), nil
		case "getcap":
			return []byte(""), nil
		default:
			return nil, nil
		}
	}

	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux",
		ExecCmd:      execFake,
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte("1024\n"), nil
			}
			return []byte("CapBnd:\t000001ffffffffff\n"), nil
		},
		LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
		Lstat:      os.Lstat,
		IsRoot:     func() bool { return true },
	}

	// Non-interactive run without preselected route must refuse cleanly
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:       "linux",
		NonInteractive: true,
		IsRoot:         func() bool { return true },
		PortChecker:    &PortChecker{Platform: "linux", ExecCmd: execFake},
		PlatformCfg:    cfg,
		FirewallMgr: &FirewallManager{
			Platform: "linux",
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			ExecCmd:  execFake,
		},
	})
	if err == nil {
		t.Fatal("expected error for non-interactive run without route")
	}
	if !strings.Contains(err.Error(), "interactive TTY") && !strings.Contains(err.Error(), "unattended") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExtenderSetupSysctlConsentRequired(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux",
		ExecCmd:      func(string, ...string) ([]byte, error) { return nil, nil },
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte("1024\n"), nil
			}
			return []byte("CapBnd:\t000001ffffffffff\n"), nil
		},
		LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
		Lstat:      os.Lstat,
		IsRoot:     func() bool { return true },
	}

	// User declines in route prompt
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		IsRoot:      func() bool { return true },
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		PlatformCfg: cfg,
		FirewallMgr: &FirewallManager{
			Platform: "linux",
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			ExecCmd:  func(string, ...string) ([]byte, error) { return nil, nil },
		},
		SelectRouteFunc: func() (string, error) {
			return "", fmt.Errorf("user declined")
		},
	})
	if err == nil {
		t.Fatal("expected error when user declines route selection")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected cancellation error, got: %v", err)
	}
}

func TestExtenderSetupSysctlIdempotence(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	mutatingCalled := false
	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "setcap" || name == "sysctl" || name == "sh" {
				mutatingCalled = true
			}
			if name == "sudo" {
				return []byte("extender-bind-probe: proven\n"), nil
			}
			return nil, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte("443\n"), nil
			}
			return nil, os.ErrNotExist
		},
		LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
		Lstat:      os.Lstat,
		IsRoot:     func() bool { return true },
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		Yes:         true,
		IsRoot:      func() bool { return true },
		Stdout:      &out,
		Route:       "sysctl",
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		PlatformCfg: cfg,
		FirewallMgr: &FirewallManager{
			Platform: "linux",
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			ExecCmd:  func(string, ...string) ([]byte, error) { return nil, nil },
		},
	})
	if err != nil {
		t.Fatalf("RunExtenderSetup failed: %v", err)
	}
	if mutatingCalled {
		t.Fatal("expected no mutating commands when already configured with sysctl 443")
	}
	if !strings.Contains(out.String(), "already configured") || !strings.Contains(out.String(), "without a file capability") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestExtenderSetupSysctlAlreadyLowerValue(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	mutatingCalled := false
	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "setcap" || name == "sysctl" || name == "sh" {
				mutatingCalled = true
			}
			if name == "sudo" {
				return []byte("extender-bind-probe: proven\n"), nil
			}
			return nil, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte("80\n"), nil
			}
			return nil, os.ErrNotExist
		},
		LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
		Lstat:      os.Lstat,
		IsRoot:     func() bool { return true },
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		Yes:         true,
		IsRoot:      func() bool { return true },
		Stdout:      &out,
		Route:       "sysctl",
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		PlatformCfg: cfg,
		FirewallMgr: &FirewallManager{
			Platform: "linux",
			LookPath: func(string) (string, error) { return "", os.ErrNotExist },
			ExecCmd:  func(string, ...string) ([]byte, error) { return nil, nil },
		},
	})
	if err != nil {
		t.Fatalf("RunExtenderSetup failed: %v", err)
	}
	if mutatingCalled {
		t.Fatal("expected no mutating commands when already lower value (80)")
	}
	if !strings.Contains(out.String(), "already lower than 443") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestExtenderSetupSysctlNonLinuxNoOp(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{PlatformName: platform}
			cmds, err := cfg.PlanRequirement("/path/provider", "user")
			if platform == "freebsd" && (err != nil || len(cmds) != 0) {
				t.Fatalf("freebsd plan requirement: cmds=%v err=%v", cmds, err)
			}
			route := detectBindRouteWith(platform, "/path/provider", "user", nil, nil)
			if route != "" {
				t.Fatalf("expected empty route for %s, got %q", platform, route)
			}
		})
	}
}
