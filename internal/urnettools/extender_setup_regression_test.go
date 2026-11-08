package urnettools

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type mockPlatformConfigurer struct {
	platform       string
	configured     bool
	checkDesc      string
	applyCalled    bool
	applyDesc      string
	applyErr       error
	verifyCalled   bool
	verifyBindErr  error
	checkCallCount int
}

func (m *mockPlatformConfigurer) Platform() string {
	return m.platform
}

func (m *mockPlatformConfigurer) CheckRequirement(binary, user string) (bool, string, error) {
	m.checkCallCount++
	return m.configured, m.checkDesc, nil
}

func (m *mockPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	m.applyCalled = true
	if m.applyErr != nil {
		return false, "", m.applyErr
	}
	return true, m.applyDesc, nil
}

func (m *mockPlatformConfigurer) VerifyBind(binary, user string) (bool, error) {
	m.verifyCalled = true
	if m.verifyBindErr != nil {
		return false, m.verifyBindErr
	}
	return true, nil
}

func TestExtenderSetup_TCP443Busy(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ss" && len(args) > 0 && args[0] == "-tlnp" {
				return []byte("LISTEN 0 4096 0.0.0.0:443 0.0.0.0:* users:((\"caddy\",pid=4567,fd=5))\n"), nil
			}
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
		applyDesc:  "applied cap_net_bind_service",
	}

	var stdout, stderr bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		PortChecker: checker,
		PlatformCfg: platformCfg,
		IsRoot:      func() bool { return true },
		Stdout:      &stdout,
		Stderr:      &stderr,
		Yes:         true,
	})
	if err == nil {
		t.Fatal("expected error when TCP 443 is busy, got nil")
	}
	for _, want := range []string{"caddy", "4567", "cannot run without it"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
	if platformCfg.applyCalled {
		t.Fatal("ApplyRequirement was called; must change nothing when port 443 is held")
	}
}

func TestExtenderSetup_UDPBusyOnly(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ss" && len(args) > 0 && args[0] == "-tlnp" {
				return []byte("LISTEN 0 4096 127.0.0.1:8080 0.0.0.0:*\n"), nil
			}
			if name == "ss" && len(args) > 0 && args[0] == "-ulnp" {
				return []byte("UNCONN 0 0 0.0.0.0:443 0.0.0.0:* users:((\"quic-server\",pid=8910,fd=7))\n" +
					"UNCONN 0 0 0.0.0.0:4053 0.0.0.0:* users:((\"dns-server\",pid=8911,fd=8))\n"), nil
			}
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
		applyDesc:  "applied cap_net_bind_service",
	}

	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) {
			if file == "firewall-cmd" {
				return file, nil
			}
			return "", errors.New("not found")
		},
		ExecCmd: func(_ string, args ...string) ([]byte, error) {
			if args[0] == "--state" {
				return []byte("running"), nil
			}
			return []byte("yes"), nil
		},
	}

	var stdout, stderr bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		PortChecker: checker,
		PlatformCfg: platformCfg,
		FirewallMgr: fwMgr,
		IsRoot:      func() bool { return true },
		Stdout:      &stdout,
		Stderr:      &stderr,
		Yes:         true,
	})
	if err != nil {
		t.Fatalf("expected success with UDP busy, got err: %v", err)
	}
	if !strings.Contains(stderr.String(), "UDP 443") || !strings.Contains(stderr.String(), "UDP 4053") {
		t.Fatalf("stderr = %q; expected warnings for UDP 443 and 4053", stderr.String())
	}
	if !platformCfg.applyCalled || !platformCfg.verifyCalled {
		t.Fatalf("apply=%v verify=%v", platformCfg.applyCalled, platformCfg.verifyCalled)
	}
}

func TestExtenderSetup_AlreadyConfigured(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd:  func(string, ...string) ([]byte, error) { return nil, nil },
	}
	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: true,
		checkDesc:  "cap_net_bind_service=+ep already set",
	}
	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) {
			if file == "ufw" {
				return file, nil
			}
			return "", errors.New("not found")
		},
		ExecCmd: func(string, ...string) ([]byte, error) {
			return []byte("Status: active\n443/tcp ALLOW Anywhere\n443/udp ALLOW Anywhere\n4053/udp ALLOW Anywhere\n"), nil
		},
	}
	var stdout, stderr bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:       "linux",
		PortChecker:    checker,
		PlatformCfg:    platformCfg,
		FirewallMgr:    fwMgr,
		IsRoot:         func() bool { return false },
		NonInteractive: true,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatalf("already configured should succeed without root or confirmation: %v", err)
	}
	if platformCfg.applyCalled {
		t.Fatal("ApplyRequirement called when already configured")
	}
	if !strings.Contains(stdout.String(), "already configured") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestExtenderSetupHiddenPortHolderDefersToElevation(t *testing.T) {
	ssOut := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 4096 *:443 *:*\n"
	run := func(root bool) error {
		cfg := &mockPlatformConfigurer{platform: "linux", configured: true}
		return RunExtenderSetup(ExtenderSetupOptions{
			Platform: "linux", PlatformCfg: cfg, IsRoot: func() bool { return root },
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Yes: true, Force: true,
			LookPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
				if args[0] == "-tlnp" {
					return []byte(ssOut), nil
				}
				return nil, nil
			}},
			FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }},
		})
	}
	if err := run(false); err == nil || strings.Contains(err.Error(), "port TCP 443 is held") || !strings.Contains(err.Error(), "requires root privileges") {
		t.Fatalf("hidden holder aborted setup before elevation: %v", err)
	}
	if err := run(true); err == nil || !strings.Contains(err.Error(), "port TCP 443 is held") {
		t.Fatalf("root run must still report the conflict: %v", err)
	}
}

func TestExtenderFirewallUFWUnreadableIsNotNoFirewall(t *testing.T) {
	fm := &FirewallManager{Platform: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ufw" {
				return name, nil
			}
			return "", errors.New("missing")
		},
		ExecCmd: func(string, ...string) ([]byte, error) {
			return []byte("ERROR: You need to be root to run this script"), errors.New("exit 1")
		}}
	st := fm.Detect()
	if st.Kind != FirewallUFW || !st.Active || st.PortsOpen {
		t.Fatalf("unreadable ufw misclassified: %+v", st)
	}
}

type nnpConfigurer struct {
	mockPlatformConfigurer
	nnp bool
}

func (n *nnpConfigurer) VerifyBind(string, string) (bool, error) { return true, nil }
func (n *nnpConfigurer) UnitNoNewPrivileges(string) (bool, error) {
	return n.nnp, nil
}

func TestReportExtenderBindWarnsWhenUnitSetsNoNewPrivileges(t *testing.T) {
	user := Provider{Unit: "urnetwork-provider.service", User: "provider", Binary: "/usr/local/bin/urnetwork"}
	cases := []struct {
		name string
		cfg  *nnpConfigurer
		p    Provider
		warn bool
	}{
		{"non-root with NoNewPrivileges", &nnpConfigurer{nnp: true}, user, true},
		{"non-root without it", &nnpConfigurer{nnp: false}, user, false},
		{"root needs no capability", &nnpConfigurer{nnp: true}, Provider{Unit: user.Unit, User: "root"}, false},
		{"no unit", &nnpConfigurer{nnp: true}, Provider{User: "provider"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := reportExtenderBind(&out, c.cfg, c.p, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "verified TCP 443 listener") {
				t.Fatalf("missing verified line: %q", out.String())
			}
			if got := strings.Contains(out.String(), "NoNewPrivileges"); got != c.warn {
				t.Fatalf("warned=%v want %v: %q", got, c.warn, out.String())
			}
		})
	}
}

func TestExtenderSetupWindowsNonAdminDoesNotSuggestSudo(t *testing.T) {
	var errOut bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform: "windows", PlatformCfg: &mockPlatformConfigurer{platform: "windows"},
		IsRoot: func() bool { return false }, Stdout: &bytes.Buffer{}, Stderr: &errOut,
		LookPath:    func(string) (string, error) { return "C:\\sudo.exe", nil },
		PortChecker: &PortChecker{Platform: "windows", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		FirewallMgr: &FirewallManager{Platform: "windows", ExecCmd: func(string, ...string) ([]byte, error) { return nil, errors.New("no rule") }},
	})
	if err == nil {
		t.Fatal("a non-admin Windows run must not proceed")
	}
	for _, text := range []string{err.Error(), errOut.String()} {
		if strings.Contains(text, "sudo") || !strings.Contains(text, "Administrator") || !strings.Contains(text, "extender setup") {
			t.Fatalf("message = %q", text)
		}
	}
}

type recordingConfigurer struct {
	mockPlatformConfigurer
	applied []string
}

func (r *recordingConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	r.applied = append(r.applied, binary+"|"+user)
	return true, "applied", nil
}

func TestExtenderSetupConfiguresEveryProviderWithoutFlags(t *testing.T) {
	old := setupDiscoverFn
	defer func() { setupDiscoverFn = old }()
	setupDiscoverFn = func() []Provider {
		return []Provider{
			{Unit: "a.service", User: "alice", Binary: "/opt/a/urnetwork"},
			{Unit: "b.service", User: "bob", Binary: "/opt/b/urnetwork"},
			{Unit: "c.service", User: "bob", Binary: "/opt/b/urnetwork"},
		}
	}
	cfg := &recordingConfigurer{mockPlatformConfigurer: mockPlatformConfigurer{platform: "linux"}}
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return true }
	opts.Yes = true
	opts.PlatformCfg = cfg
	opts.FirewallMgr = &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }}
	err := RunExtenderSetup(opts)
	if err != nil {
		t.Fatalf("multi-provider host failed: %v", err)
	}
	if want := []string{"/opt/a/urnetwork|alice", "/opt/b/urnetwork|bob"}; !reflect.DeepEqual(cfg.applied, want) {
		t.Fatalf("applied to %v, want %v", cfg.applied, want)
	}
}

func TestExtenderBindOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		execErr   error
		want      bool
		failed    bool
	}{
		{"proven", "extender-bind-probe: proven", nil, true, false},
		{"failed", "extender-bind-probe: failed: permission denied", errors.New("exit 1"), false, true},
		{"unverified", "sudo denied", errors.New("exit 1"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{PlatformName: "linux", LookupPath: func(string) (string, error) { return "sudo", nil }, ExecCmd: func(name string, args ...string) ([]byte, error) {
				want := []string{"-n", "-u", "provider", "--", "/provider", "--extender-bind-probe"}
				if name != "sudo" || !reflect.DeepEqual(args, want) {
					t.Fatalf("command %s %v", name, args)
				}
				return []byte(tc.out), tc.execErr
			}}
			ok, err := sp.VerifyBind("/provider", "provider")
			if ok != tc.want || (err != nil) != tc.failed {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			var out bytes.Buffer
			err = reportExtenderBind(&out, sp, Provider{Binary: "/provider", User: "provider"}, false)
			if (err != nil) != tc.failed {
				t.Fatalf("report err=%v", err)
			}
			if !tc.failed && !strings.Contains(out.String(), tc.name) && !(tc.want && strings.Contains(out.String(), "verified")) {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func setupRegressionOptions() ExtenderSetupOptions {
	return ExtenderSetupOptions{
		Platform:    "linux",
		PlatformCfg: &mockPlatformConfigurer{platform: "linux"},
		IsRoot:      func() bool { return false },
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }},
	}
}

func TestExtenderSetupFirewallApplyFailure(t *testing.T) {
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return true }
	opts.Yes = true
	failure := errors.New("denied firewall")
	opts.FirewallMgr = &FirewallManager{Platform: "linux", LookPath: func(name string) (string, error) {
		if name == "firewall-cmd" {
			return name, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--query-port=") {
				return []byte("no"), errors.New("not enabled")
			}
		}
		if args[0] == "--permanent" {
			return nil, failure
		}
		return []byte("ok"), nil
	}}
	err := RunExtenderSetup(opts)
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
}

func TestApplyLinuxRefusesANonRegularBinary(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "urnetwork-real")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "urnetwork")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	for name, tc := range map[string]struct {
		binary  string
		wantErr string
	}{
		"symlink":   {link, "not a regular file"},
		"directory": {dir, "not a regular file"},
		"missing":   {filepath.Join(dir, "gone"), "cannot apply"},
	} {
		t.Run(name, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{
				PlatformName: "linux",
				Lstat:        os.Lstat,
				ExecCmd: func(n string, args ...string) ([]byte, error) {
					t.Fatalf("%s ran %v on %s", n, args, tc.binary)
					return nil, nil
				},
			}
			_, _, err := sp.applyLinux(tc.binary)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestApplyLinuxRejectsBlockedBoundingSet(t *testing.T) {
	for _, tc := range []struct {
		name, mask string
		allowed    bool
	}{
		{"blocked", "00000000a80421fb", false},
		{"allowed", "00000000a80425fb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{PlatformName: "linux"}
			cfg.ReadFile = func(string) ([]byte, error) { return []byte("CapBnd:\t" + tc.mask + "\n"), nil }
			var set bool
			cfg.ExecCmd = func(name string, args ...string) ([]byte, error) {
				switch name {
				case "setcap":
					set = true
					return nil, nil
				case "getcap":
					return []byte("/binary cap_net_bind_service=ep"), nil
				default:
					return nil, fmt.Errorf("unexpected command %s", name)
				}
			}
			_, _, err := cfg.applyLinux("/binary")
			if set != tc.allowed {
				t.Fatalf("setcap called=%v want %v", set, tc.allowed)
			}
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && (err == nil || !strings.Contains(err.Error(), "bounding set")) {
				t.Fatalf("missing bounding-set error: %v", err)
			}
		})
	}
}

func TestExtenderFreeBSDMissingSysctlConfIsCreated(t *testing.T) {
	var written string
	sp := &SystemPlatformConfigurer{PlatformName: "freebsd",
		ReadFile: func(string) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "/etc/sysctl.conf", Err: fs.ErrNotExist}
		},
		WriteFile: func(_ string, d []byte, _ os.FileMode) error { written = string(d); return nil },
		ExecCmd:   func(string, ...string) ([]byte, error) { return []byte("0\n"), nil }}
	if ok, _, err := sp.applyFreeBSD("alice"); err != nil || !ok || !strings.Contains(written, "net.inet.ip.portrange.reservedhigh=0") {
		t.Fatalf("ok=%v err=%v written=%q", ok, err, written)
	}
	sp.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	written = ""
	if _, _, err := sp.applyFreeBSD("alice"); err == nil || written != "" {
		t.Fatalf("unreadable sysctl.conf: err=%v written=%q", err, written)
	}
}

func TestExtenderWindowsDisabledRuleIsRepaired(t *testing.T) {
	show := func(name, enabled, action string) []byte {
		return []byte("Rule Name:        " + name + "\n----\nEnabled:          " + enabled + "\nDirection:        In\nAction:           " + action + "\n")
	}
	state := map[string][2]string{
		"URnetwork Extender":     {"Yes", "Allow"},
		"URnetwork Extender UDP": {"No", "Allow"},
	}
	var calls []string
	run := func(_ string, args ...string) ([]byte, error) {
		name := strings.TrimPrefix(args[4], "name=")
		calls = append(calls, args[2]+" "+name)
		switch args[2] {
		case "show":
			st, ok := state[name]
			if !ok {
				return nil, errors.New("no rules match")
			}
			return show(name, st[0], st[1]), nil
		case "delete":
			delete(state, name)
		case "add":
			state[name] = [2]string{"Yes", "Allow"}
		}
		return nil, nil
	}
	fm := &FirewallManager{Platform: "windows", ExecCmd: run}
	if fm.Detect().PortsOpen {
		t.Fatal("a disabled UDP rule was reported open")
	}
	calls = nil
	applied, err := fm.Apply(fm.Detect())
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	var mutating []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "show ") {
			mutating = append(mutating, c)
		}
	}
	if !reflect.DeepEqual(mutating, []string{"delete URnetwork Extender UDP", "add URnetwork Extender UDP"}) {
		t.Fatalf("mutations = %v", mutating)
	}
	if !fm.Detect().PortsOpen {
		t.Fatal("repaired rules not reported open")
	}
}
