package urnettools

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExtenderSysctlSelectedPort(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		selected, live, persisted int
		want                      bool
	}{
		{"default", 0, 443, 443, true},
		{"dns53", 53, 53, 53, true},
		{"dns53 live too high", 53, 443, 53, false},
		{"dns53 persisted too high", 53, 53, 443, false},
		{"dns53 lower", 53, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", SysctlPort: tc.selected,
				ReadFile: func(path string) ([]byte, error) {
					switch path {
					case "/proc/sys/net/ipv4/ip_unprivileged_port_start":
						return []byte(fmt.Sprint(tc.live)), nil
					case extenderSysctlDropin:
						return []byte(fmt.Sprintf("net.ipv4.ip_unprivileged_port_start=%d\n", tc.persisted)), nil
					default:
						return nil, os.ErrNotExist
					}
				},
			}
			got, _, err := cfg.CheckRequirement("/provider", "provider")
			if err != nil || got != tc.want {
				t.Fatalf("CheckRequirement = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestExtenderSysctlPort53RecoveryRestoresPrior443(t *testing.T) {
	cfg := &SystemPlatformConfigurer{
		PlatformName: "linux", Route: "sysctl", SysctlPort: 53,
		ReadFile: func(path string) ([]byte, error) {
			switch path {
			case "/proc/sys/net/ipv4/ip_unprivileged_port_start":
				return []byte("443\n"), nil
			case extenderSysctlDropin:
				return []byte(extenderSysctlContent), nil
			default:
				return nil, os.ErrNotExist
			}
		},
	}
	got, err := cfg.sysctlRecovery()
	want := "sudo mv " + extenderSysctlBackup + " " + extenderSysctlDropin + " && sudo sysctl -w net.ipv4.ip_unprivileged_port_start=443"
	if err != nil || got != want {
		t.Fatalf("recovery = %q, %v; want %q", got, err, want)
	}
}

func TestExtenderSysctlPort53PlanAndFallback(t *testing.T) {
	var written string
	var calls []string
	live := "1024"
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", SysctlPort: 53,
		ReadFile: func(path string) ([]byte, error) {
			if path == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte(live), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(_ string, data []byte, _ os.FileMode) error { written = string(data); return nil },
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			calls = append(calls, setupCommand{name: name, args: args}.String())
			if len(args) > 0 && args[0] == "--system" {
				return nil, fmt.Errorf("test failure")
			}
			live = "53"
			return nil, nil
		},
	}
	plan, err := cfg.PlanRequirement("/provider", "provider")
	if err != nil || len(plan) != 2 || !strings.Contains(plan[0].String(), "start=53") {
		t.Fatalf("plan=%v err=%v", plan, err)
	}
	applied, _, err := cfg.ApplyRequirement("/provider", "provider")
	if err != nil || !applied || written != "net.ipv4.ip_unprivileged_port_start=53\n" {
		t.Fatalf("applied=%v written=%q err=%v", applied, written, err)
	}
	if len(calls) != 2 || calls[1] != "sysctl -w net.ipv4.ip_unprivileged_port_start=53" {
		t.Fatalf("calls=%v", calls)
	}
}
