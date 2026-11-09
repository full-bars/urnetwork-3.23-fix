package urnettools

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderSetupConfirmation(t *testing.T) {
	plan := []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", "/path with spaces/provider"}}, {name: "ufw", args: []string{"allow", "443/udp"}}}
	for _, tc := range []struct {
		name                 string
		yes, dryRun, confirm bool
		confirmErr           error
		wantCalls            int
		wantErr              bool
	}{
		{name: "non-interactive", confirmErr: errors.New("not a terminal"), wantErr: true},
		{name: "declined", wantErr: true},
		{name: "confirmed", confirm: true, wantCalls: 2},
		{name: "explicit yes", yes: true, wantCalls: 2},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var ran []setupCommand
			err := executeExtenderSetupPlan(&out, plan, tc.yes, tc.dryRun, func() (bool, error) { return tc.confirm, tc.confirmErr }, func(c setupCommand) error {
				ran = append(ran, c)
				return nil
			})
			if (err != nil) != tc.wantErr || len(ran) != tc.wantCalls {
				t.Fatalf("err=%v commands=%v", err, ran)
			}
			want := "Commands to run:\n  setcap cap_net_bind_service=+ep '/path with spaces/provider'\n  ufw allow 443/udp\n"
			if out.String() != want {
				t.Fatalf("printed %q, want %q", out.String(), want)
			}
			if tc.wantCalls > 0 && !reflect.DeepEqual(ran, plan) {
				t.Fatalf("executed %v, want %v", ran, plan)
			}
			if tc.confirmErr != nil && !strings.Contains(err.Error(), "--yes") {
				t.Fatalf("unclear error: %v", err)
			}
		})
	}
}

func TestRunExtenderSetupConfirmFirstAndUFW(t *testing.T) {
	// An elevated rerun must confirm its newly computed plan, unless the
	// operator explicitly supplied --yes or --force.
	t.Setenv(urnetElevatedEnv, "1")
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "provider", Binary: bin, Running: true, PID: 100}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	ufwStatus := `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW       Anywhere
`

	for _, tc := range []struct {
		name           string
		yes            bool
		force          bool
		nonInteractive bool
		confirm        func() (bool, error)
		wantErr        bool
		wantMutated    bool
	}{
		{
			name:           "unconfirmed non-interactive makes zero mutating calls",
			nonInteractive: true,
			wantErr:        true,
			wantMutated:    false,
		},
		{
			name:        "declined interactive makes zero mutating calls",
			confirm:     func() (bool, error) { return false, nil },
			wantErr:     true,
			wantMutated: false,
		},
		{
			name:        "confirmed interactive executes exact printed commands",
			confirm:     func() (bool, error) { return true, nil },
			wantErr:     false,
			wantMutated: true,
		},
		{
			name:           "explicit yes executes exact printed commands",
			yes:            true,
			nonInteractive: true,
			wantErr:        false,
			wantMutated:    true,
		},
		{
			name:           "force flag executes exact printed commands",
			force:          true,
			nonInteractive: true,
			wantErr:        false,
			wantMutated:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capSet := false
			var mutating []string
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
				case "ufw":
					if len(args) == 1 && args[0] == "status" {
						return []byte(ufwStatus), nil
					}
					mutating = append(mutating, cmdStr)
					return nil, nil
				case "sudo":
					return []byte("extender-bind-probe: proven\n"), nil
				case "systemctl":
					return []byte("no\n"), nil
				default:
					t.Fatalf("unexpected command: %s %v", name, args)
					return nil, nil
				}
			}

			var out, errOut bytes.Buffer
			err := RunExtenderSetup(ExtenderSetupOptions{
				Platform:       "linux",
				Yes:            tc.yes,
				Force:          tc.force,
				NonInteractive: tc.nonInteractive,
				ConfirmFunc:    tc.confirm,
				IsRoot:         func() bool { return true },
				Stdout:         &out,
				Stderr:         &errOut,
				PortChecker:    &PortChecker{Platform: "linux", ExecCmd: execFake},
				PlatformCfg: &SystemPlatformConfigurer{
					PlatformName: "linux",
					ExecCmd:      execFake,
					ReadFile:     func(string) ([]byte, error) { return []byte("CapBnd:\t000001ffffffffff\n"), nil },
					LookupPath:   func(string) (string, error) { return "/usr/bin/sudo", nil },
					Lstat:        os.Lstat,
					IsRoot:       func() bool { return true },
				},
				FirewallMgr: &FirewallManager{
					Platform: "linux",
					LookPath: func(file string) (string, error) {
						if file == "ufw" {
							return "/usr/sbin/ufw", nil
						}
						return "", errors.New("not found")
					},
					ExecCmd: execFake,
				},
			})

			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v (out=%q)", err, tc.wantErr, out.String())
			}

			printed := parsePrintedSetupCommands(out.String())
			wantCommands := []string{
				setupCommand{name: "setcap", args: []string{"cap_net_bind_service=+ep", bin}}.String(),
				"ufw allow 443/udp",
				"ufw allow 4053/udp",
			}
			if !reflect.DeepEqual(printed, wantCommands) {
				t.Fatalf("printed commands = %v, want %v\nfull output:\n%s", printed, wantCommands, out.String())
			}
			if !strings.Contains(out.String(), "tcp 443 (required") || !strings.Contains(out.String(), "udp 443 (optional") || !strings.Contains(out.String(), "udp 4053 (optional") {
				t.Fatalf("output missing port explanation:\n%s", out.String())
			}

			if !tc.wantMutated {
				if len(mutating) != 0 {
					t.Fatalf("expected zero mutating calls when unconfirmed, got %v", mutating)
				}
				if tc.nonInteractive && !strings.Contains(err.Error(), "--yes") {
					t.Fatalf("expected error to mention --yes, got %v", err)
				}
			} else {
				if !reflect.DeepEqual(mutating, printed) {
					t.Fatalf("executed commands %v != printed commands %v", mutating, printed)
				}
				if !strings.Contains(out.String(), "verified TCP 443 listener as provider user") {
					t.Fatalf("missing bind verification output:\n%s", out.String())
				}
			}
		})
	}
}

func parsePrintedSetupCommands(output string) []string {
	var cmds []string
	inBlock := false
	for _, line := range strings.Split(output, "\n") {
		if line == "Commands to run:" {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.HasPrefix(line, "  ") {
				cmds = append(cmds, strings.TrimPrefix(line, "  "))
			} else {
				break
			}
		}
	}
	return cmds
}

func TestExtenderSetupFlagsAndDispatch(t *testing.T) {
	for _, flag := range []string{"-y", "--yes", "-f", "--force"} {
		yes, dryRun, _, _, err := parseExtenderSetupArgs([]string{"setup", flag})
		if err != nil || !yes || dryRun {
			t.Fatalf("flag %s: yes=%v dryRun=%v err=%v", flag, yes, dryRun, err)
		}
	}
	for _, flag := range []string{"-n", "--dry-run"} {
		yes, dryRun, _, _, err := parseExtenderSetupArgs([]string{"setup", flag})
		if err != nil || yes || !dryRun {
			t.Fatalf("flag %s: yes=%v dryRun=%v err=%v", flag, yes, dryRun, err)
		}
	}
	yes, dryRun, _, target, err := parseExtenderSetupArgs([]string{"setup", "--unit", "urnetwork.service", "--yes"})
	if err != nil || !yes || dryRun || target.Unit != "urnetwork.service" {
		t.Fatalf("target parse: yes=%v dryRun=%v target=%+v err=%v", yes, dryRun, target, err)
	}
	for _, route := range []string{"sysctl", "capability", "skip"} {
		yes, dryRun, r, _, err := parseExtenderSetupArgs([]string{"setup", "--route=" + route})
		if err != nil || yes || dryRun || r != route {
			t.Fatalf("route flag %s: route=%q err=%v", route, r, err)
		}
	}
	for _, bad := range []string{"--enable", "-e", "--json", "--non-interactive", "--route=invalid", "positional"} {
		if _, _, _, _, err := parseExtenderSetupArgs([]string{"setup", bad}); err == nil {
			t.Fatalf("setup accepted invalid argument %q", bad)
		}
	}
	for _, sub := range []string{"status", "about", "on", "off"} {
		if err := Run([]string{"extender", sub, "--force"}); err == nil {
			t.Fatalf("extender %s silently accepted --force", sub)
		}
	}
}
