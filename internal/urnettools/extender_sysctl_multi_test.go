package urnettools

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeSysctlInfo struct{ name string }

func (f fakeSysctlInfo) Name() string     { return f.name }
func (fakeSysctlInfo) Size() int64        { return 0 }
func (fakeSysctlInfo) Mode() os.FileMode  { return 0o644 }
func (fakeSysctlInfo) ModTime() time.Time { return time.Time{} }
func (fakeSysctlInfo) IsDir() bool        { return false }
func (fakeSysctlInfo) Sys() any           { return nil }

// The sysctl route is host-wide, so several providers share one write. A
// second apply would see the drop-in setup itself just wrote: it would either
// back that file up over the real prior state or refuse because the backup
// already exists. The recovery must keep restoring the true prior state.
func TestExtenderSetupSysctlRouteAppliesOnceForSeveralProviders(t *testing.T) {
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{
			{Unit: "a.service", StateDir: "/state/a", User: "alice", Binary: "/opt/a/provider"},
			{Unit: "b.service", StateDir: "/state/b", User: "bob", Binary: "/opt/b/provider"},
		}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	const priorContent = "net.ipv4.ip_unprivileged_port_start=1024\n# kept from before setup\n"
	const liveFile = "/proc/sys/net/ipv4/ip_unprivileged_port_start"
	const unprivKey = "net.ipv4.ip_unprivileged_port_start"

	for _, tc := range []struct {
		name         string
		existing     bool
		wantBackups  int
		wantRecovery string
	}{
		{name: "no prior drop-in", wantRecovery: "sudo rm " + extenderSysctlDropin + " && sudo sysctl -w " + unprivKey + "=1024"},
		{name: "prior drop-in kept once", existing: true, wantBackups: 1, wantRecovery: "sudo mv " + extenderSysctlBackup + " " + extenderSysctlDropin + " && sudo sysctl -w " + unprivKey + "=1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			live := "1024\n"
			files := map[string]string{}
			if tc.existing {
				files[extenderSysctlDropin] = priorContent
			}
			writes, backups, applies, setcaps := 0, 0, 0, 0
			execFake := func(name string, args ...string) ([]byte, error) {
				switch name {
				case "ss":
					return []byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"), nil
				case "cp":
					backups++
					files[extenderSysctlBackup] = files[extenderSysctlDropin]
				case "sysctl":
					if len(args) == 1 && args[0] == "--system" {
						applies++
						live = "443\n"
					}
				case "setcap":
					setcaps++
				case "getcap":
					return nil, nil
				case "sudo":
					return []byte("extender-bind-probe: proven\n"), nil
				}
				return nil, nil
			}
			cfg := &SystemPlatformConfigurer{
				PlatformName: "linux",
				ExecCmd:      execFake,
				ReadFile: func(path string) ([]byte, error) {
					if path == liveFile {
						return []byte(live), nil
					}
					if data, ok := files[path]; ok {
						return []byte(data), nil
					}
					return nil, os.ErrNotExist
				},
				WriteFile: func(name string, data []byte, perm os.FileMode) error {
					if name == extenderSysctlDropin {
						writes++
					}
					files[name] = string(data)
					return nil
				},
				Lstat: func(name string) (os.FileInfo, error) {
					if _, ok := files[name]; ok {
						return fakeSysctlInfo{name: name}, nil
					}
					return nil, os.ErrNotExist
				},
				LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			}
			err := RunExtenderSetup(ExtenderSetupOptions{
				Platform:    "linux",
				Route:       "sysctl",
				Yes:         true,
				IsRoot:      func() bool { return true },
				Stdout:      &out,
				Stderr:      &errOut,
				PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFake},
				PlatformCfg: cfg,
				FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", os.ErrNotExist }, ExecCmd: execFake},
			})
			if err != nil {
				t.Fatalf("RunExtenderSetup failed: %v\n%s", err, out.String())
			}
			if writes != 1 || backups != tc.wantBackups || applies != 1 {
				t.Fatalf("writes=%d backups=%d (want %d) applies=%d, want one write and one apply", writes, backups, tc.wantBackups, applies)
			}
			if setcaps != 0 {
				t.Fatalf("the sysctl route must not run setcap, ran %d", setcaps)
			}
			if tc.existing {
				if got := files[extenderSysctlBackup]; got != priorContent {
					t.Fatalf("backup content %q, want the original %q", got, priorContent)
				}
			} else if _, ok := files[extenderSysctlBackup]; ok {
				t.Fatalf("no prior drop-in, yet a backup of setup's own file was left behind")
			}
			if got := files[extenderSysctlDropin]; got != extenderSysctlContent {
				t.Fatalf("drop-in content %q, want %q", got, extenderSysctlContent)
			}
			if got := strings.Count(out.String(), "Recovery command: "+tc.wantRecovery); got != 1 {
				t.Fatalf("want the recovery command once (%q), got %d:\n%s", tc.wantRecovery, got, out.String())
			}
			if strings.Count(out.String(), "applied sysctl drop-in") != 1 {
				t.Fatalf("the route must be reported applied exactly once:\n%s", out.String())
			}
		})
	}
}
