package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sysctlRoot builds a fake root with the given files and returns a configurer
// whose file access is redirected into it. The live value is 443.
func sysctlRoot(t *testing.T, files map[string]string) *SystemPlatformConfigurer {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &SystemPlatformConfigurer{
		PlatformName: "linux",
		Route:        "sysctl",
		ReadFile: func(p string) ([]byte, error) {
			if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
				return []byte("443"), nil
			}
			return os.ReadFile(filepath.Join(root, p))
		},
		ReadDir: func(p string) ([]os.DirEntry, error) { return os.ReadDir(filepath.Join(root, p)) },
	}
}

const sysctlKey = "net.ipv4.ip_unprivileged_port_start="

func TestExtenderSysctlScanUsrLocalLibLaterDropinWins(t *testing.T) {
	cfg := sysctlRoot(t, map[string]string{
		"etc/sysctl.d/50-early.conf":           sysctlKey + "443\n",
		"usr/local/lib/sysctl.d/90-later.conf": sysctlKey + "1024\n",
	})
	ok, desc, err := cfg.CheckRequirement("/provider", "provider")
	if err != nil || ok || !strings.Contains(desc, "not persisted") {
		t.Fatalf("later 1024 drop-in in /usr/local/lib/sysctl.d must win: ok=%v desc=%q err=%v", ok, desc, err)
	}
}

func TestExtenderSysctlScanUsrLocalLibCountsAsPersisted(t *testing.T) {
	cfg := sysctlRoot(t, map[string]string{"usr/local/lib/sysctl.d/50-x.conf": sysctlKey + "443\n"})
	if ok, desc, err := cfg.CheckRequirement("/provider", "provider"); err != nil || !ok {
		t.Fatalf("443 in /usr/local/lib/sysctl.d must persist: ok=%v desc=%q err=%v", ok, desc, err)
	}
}

func TestExtenderSysctlScanMaskingAndOrder(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"same name in earlier dir masks later 1024", map[string]string{
			"etc/sysctl.d/50-x.conf":     sysctlKey + "443\n",
			"usr/lib/sysctl.d/50-x.conf": sysctlKey + "1024\n",
		}, true},
		{"same name in earlier dir masks later 443", map[string]string{
			"etc/sysctl.d/50-x.conf":     sysctlKey + "1024\n",
			"usr/lib/sysctl.d/50-x.conf": sysctlKey + "443\n",
		}, false},
		{"lexical order across directories: later name wins", map[string]string{
			"usr/lib/sysctl.d/10-a.conf": sysctlKey + "1024\n",
			"etc/sysctl.d/20-b.conf":     sysctlKey + "443\n",
		}, true},
		{"lexical order across directories: later name loses", map[string]string{
			"etc/sysctl.d/10-a.conf": sysctlKey + "443\n",
			"run/sysctl.d/20-b.conf": sysctlKey + "1024\n",
		}, false},
		{"sysctl.conf is applied last", map[string]string{
			"etc/sysctl.d/99-z.conf": sysctlKey + "443\n",
			"etc/sysctl.conf":        sysctlKey + "1024\n",
		}, false},
		{"sysctl.conf can restore a low value", map[string]string{
			"etc/sysctl.d/10-a.conf": sysctlKey + "1024\n",
			"etc/sysctl.conf":        sysctlKey + "443\n",
		}, true},
		{"backup file without .conf is not loaded", map[string]string{
			"etc/sysctl.d/99-urnetwork-extender.conf.pre-urnetwork": sysctlKey + "443\n",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sysctlRoot(t, tc.files).sysctlPersisted(); got != tc.want {
				t.Fatalf("sysctlPersisted()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestExtenderSysctlScanInjectedPaths(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, val := range map[string]string{filepath.Join(a, "50-x.conf"): "443", filepath.Join(b, "60-y.conf"): "1024"} {
		if err := os.WriteFile(path, []byte(sysctlKey+val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &SystemPlatformConfigurer{
		ReadFile: os.ReadFile, ReadDir: os.ReadDir,
		SysctlDirs: []string{a, b}, SysctlConf: filepath.Join(dir, "missing.conf"),
	}
	if cfg.sysctlPersisted() {
		t.Fatal("later 1024 in injected dir must win")
	}
	cfg.SysctlDirs = []string{a}
	if !cfg.sysctlPersisted() {
		t.Fatal("443 alone in injected dir must persist")
	}
}
