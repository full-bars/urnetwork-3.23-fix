package urnettools

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallProviderBinaryExtenderCapability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		present     bool
		readFails   bool
		applyFails  bool
		wantSetcap  int
		wantWarning bool
	}{
		{name: "missing", wantWarning: true},
		{name: "unreadable", readFails: true, wantWarning: true},
		{name: "present", present: true, wantSetcap: 1},
		{name: "apply failure", present: true, applyFails: true, wantSetcap: 1, wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "new")
			dst := filepath.Join(dir, "provider binary's path")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			present, sets, checks := false, 0, 0
			run := func(name string, args ...string) ([]byte, error) {
				data, err := os.ReadFile(dst)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) == "old" {
					if name != "getcap" {
						t.Fatalf("unexpected %s before swap", name)
					}
					checks++
					if tc.readFails {
						return nil, fmt.Errorf("getcap unavailable")
					}
					if tc.present {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				}
				if string(data) != "new" {
					t.Fatalf("unexpected binary contents %q", data)
				}
				switch name {
				case "getcap":
					checks++
					if len(args) != 1 || args[0] != dst {
						t.Fatalf("getcap args: %q", args)
					}
					if present {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap", "sudo":
					sets++
					want := []string{"cap_net_bind_service=+ep", dst}
					if name == "sudo" {
						want = []string{"-n", "setcap", "cap_net_bind_service=+ep", dst}
					}
					if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
						t.Fatalf("setcap invocation: %s %q", name, args)
					}
					if tc.applyFails {
						return nil, fmt.Errorf("permission denied")
					}
					present = true
					return nil, nil
				default:
					t.Fatalf("unexpected command %s", name)
					return nil, nil
				}
			}
			var warnings bytes.Buffer
			if err := installProviderBinaryWithRunnerAndStatus(src, dst, "", "linux", &warnings, run, filepath.Join(dir, "missing-status")); err != nil {
				t.Fatalf("install failed: %v", err)
			}
			if checks == 0 || sets != tc.wantSetcap {
				t.Fatalf("getcap=%d setcap=%d", checks, sets)
			}
			if tc.wantSetcap == 1 && !tc.applyFails && checks != 3 {
				t.Fatalf("successful apply not verified: %d checks", checks)
			}
			if got := warnings.String(); tc.wantWarning != strings.Contains(got, "WARNING:") {
				t.Fatalf("warning=%q", got)
			}
			if tc.wantWarning {
				quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
				want := "WARNING: extender capability missing or unverified for " + fmt.Sprintf("%q", dst) + "; run: sudo setcap cap_net_bind_service=+ep " + quoted + "\n"
				if !tc.present {
					want = "WARNING: previous extender capability absent or unverified; to enable it run: sudo setcap cap_net_bind_service=+ep " + quoted + "\n"
				}
				if warnings.String() != want {
					t.Fatalf("warning=%q, want %q", warnings.String(), want)
				}
			}
		})
	}
}

func TestInstallProviderBinaryCapabilityNonLinux(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(string, ...string) ([]byte, error) {
		t.Fatal("non-Linux capability check ran a command")
		return nil, nil
	}
	if err := installProviderBinaryWithRunner(src, dst, "", "darwin", &bytes.Buffer{}, run); err != nil {
		t.Fatal(err)
	}
}

func TestExtenderSysctlRouteActive(t *testing.T) {
	origRead := updateSysctlReadFile
	t.Cleanup(func() { updateSysctlReadFile = origRead })

	for _, tc := range []struct {
		name     string
		platform string
		content  string
		readErr  error
		want     bool
	}{
		{name: "linux 443 active", platform: "linux", content: "443\n", want: true},
		{name: "linux 443 with spaces", platform: "linux", content: "  443  \n", want: true},
		{name: "linux 0 active", platform: "linux", content: "0\n", want: true},
		{name: "linux 80 active", platform: "linux", content: "80\n", want: true},
		{name: "linux 444 inactive", platform: "linux", content: "444\n", want: false},
		{name: "linux 1024 inactive", platform: "linux", content: "1024\n", want: false},
		{name: "linux negative inactive", platform: "linux", content: "-1\n", want: false},
		{name: "linux non-numeric inactive", platform: "linux", content: "invalid\n", want: false},
		{name: "linux empty inactive", platform: "linux", content: "", want: false},
		{name: "linux unreadable inactive", platform: "linux", readErr: os.ErrNotExist, want: false},
		{name: "darwin ignored", platform: "darwin", content: "443\n", want: false},
		{name: "windows ignored", platform: "windows", content: "443\n", want: false},
		{name: "freebsd ignored", platform: "freebsd", content: "443\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updateSysctlReadFile = func(path string) ([]byte, error) {
				if path != extenderSysctlPortStartPath {
					return nil, os.ErrNotExist
				}
				if tc.readErr != nil {
					return nil, tc.readErr
				}
				return []byte(tc.content), nil
			}
			if got := extenderSysctlRouteActive(tc.platform); got != tc.want {
				t.Fatalf("extenderSysctlRouteActive(%q) = %v, want %v", tc.platform, got, tc.want)
			}
		})
	}
}

func TestInstallProviderBinaryExtenderSysctlActive(t *testing.T) {
	origRead := updateSysctlReadFile
	t.Cleanup(func() { updateSysctlReadFile = origRead })

	for _, tc := range []struct {
		name          string
		sysctlContent string
		hadCap        bool
	}{
		{name: "port 443 without previous cap", sysctlContent: "443\n", hadCap: false},
		{name: "port 443 with previous cap", sysctlContent: "443\n", hadCap: true},
		{name: "port 0 without previous cap", sysctlContent: "0\n", hadCap: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updateSysctlReadFile = func(path string) ([]byte, error) {
				if path == extenderSysctlPortStartPath {
					return []byte(tc.sysctlContent), nil
				}
				return nil, os.ErrNotExist
			}

			dir := t.TempDir()
			src := filepath.Join(dir, "new")
			dst := filepath.Join(dir, "provider")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}

			checks, sets := 0, 0
			run := func(name string, args ...string) ([]byte, error) {
				switch name {
				case "getcap":
					checks++
					if tc.hadCap {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap", "sudo":
					sets++
					return nil, nil
				default:
					t.Fatalf("unexpected command %s", name)
					return nil, nil
				}
			}

			var warnings bytes.Buffer
			if err := installProviderBinaryWithRunnerAndStatus(src, dst, "", "linux", &warnings, run, filepath.Join(dir, "missing-status")); err != nil {
				t.Fatalf("install failed: %v", err)
			}

			if checks != 0 || sets != 0 {
				t.Fatalf("expected no capability checks or setcap when sysctl route is active, got checks=%d sets=%d", checks, sets)
			}
			if got := warnings.String(); got != "" {
				t.Fatalf("expected no warnings when sysctl route is active, got: %q", got)
			}
			data, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "new" {
				t.Fatalf("binary not updated: got %q, want 'new'", string(data))
			}
		})
	}
}

func TestInstallProviderBinaryExtenderSysctlInactive(t *testing.T) {
	origRead := updateSysctlReadFile
	t.Cleanup(func() { updateSysctlReadFile = origRead })

	for _, tc := range []struct {
		name          string
		sysctlContent string
		readErr       error
		hadCap        bool
		wantChecks    int
		wantSets      int
		wantWarning   bool
	}{
		{
			name:          "sysctl 1024 without previous cap warns",
			sysctlContent: "1024\n",
			hadCap:        false,
			wantChecks:    1,
			wantSets:      0,
			wantWarning:   true,
		},
		{
			name:          "sysctl 1024 with previous cap restores",
			sysctlContent: "1024\n",
			hadCap:        true,
			wantChecks:    3,
			wantSets:      1,
			wantWarning:   false,
		},
		{
			name:        "sysctl unreadable without previous cap warns",
			readErr:     os.ErrNotExist,
			hadCap:      false,
			wantChecks:  1,
			wantSets:    0,
			wantWarning: true,
		},
		{
			name:        "sysctl unreadable with previous cap restores",
			readErr:     os.ErrNotExist,
			hadCap:      true,
			wantChecks:  3,
			wantSets:    1,
			wantWarning: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updateSysctlReadFile = func(path string) ([]byte, error) {
				if path != extenderSysctlPortStartPath {
					return nil, os.ErrNotExist
				}
				if tc.readErr != nil {
					return nil, tc.readErr
				}
				return []byte(tc.sysctlContent), nil
			}

			dir := t.TempDir()
			src := filepath.Join(dir, "new")
			dst := filepath.Join(dir, "provider")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}

			checks, sets := 0, 0
			capPresent := false
			run := func(name string, args ...string) ([]byte, error) {
				data, err := os.ReadFile(dst)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) == "old" {
					checks++
					if tc.hadCap {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				}
				switch name {
				case "getcap":
					checks++
					if capPresent {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap", "sudo":
					sets++
					capPresent = true
					return nil, nil
				default:
					t.Fatalf("unexpected command %s", name)
					return nil, nil
				}
			}

			var warnings bytes.Buffer
			if err := installProviderBinaryWithRunnerAndStatus(src, dst, "", "linux", &warnings, run, filepath.Join(dir, "missing-status")); err != nil {
				t.Fatalf("install failed: %v", err)
			}

			if checks != tc.wantChecks || sets != tc.wantSets {
				t.Fatalf("checks=%d want %d, sets=%d want %d", checks, tc.wantChecks, sets, tc.wantSets)
			}
			if tc.wantWarning != strings.Contains(warnings.String(), "WARNING:") {
				t.Fatalf("warning=%q, wantWarning=%v", warnings.String(), tc.wantWarning)
			}
		})
	}
}

func TestInstallProviderBinarySysctlNonLinux(t *testing.T) {
	origRead := updateSysctlReadFile
	t.Cleanup(func() { updateSysctlReadFile = origRead })

	// Even if sysctl would report 443, non-Linux must ignore it and not invoke runner.
	updateSysctlReadFile = func(path string) ([]byte, error) {
		return []byte("443\n"), nil
	}

	for _, platform := range []string{"darwin", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "new")
			dst := filepath.Join(dir, "provider")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			run := func(name string, args ...string) ([]byte, error) {
				t.Fatalf("non-Linux platform %s ran command: %s", platform, name)
				return nil, nil
			}
			var warnings bytes.Buffer
			if err := installProviderBinaryWithRunner(src, dst, "", platform, &warnings, run); err != nil {
				t.Fatal(err)
			}
			if warnings.Len() != 0 {
				t.Fatalf("unexpected warnings: %s", warnings.String())
			}
			data, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "new" {
				t.Fatalf("binary not installed: %q", string(data))
			}
		})
	}
}
