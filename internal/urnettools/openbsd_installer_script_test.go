package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBSDToolLinkFailurePreservesProvider(t *testing.T) {
	skipWithoutPOSIXShell(t)
	data, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	link := strings.Index(script, `    link_tool || { rm -rf "$tmpdir"; exit 1; }`)
	swap := strings.Index(script, `    priv mv -f "$staged" "$provider_bin"`)
	if link < 0 || swap < 0 || link > swap {
		t.Fatal("tool link failure must abort before replacing the provider")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "provider")
	if err := os.WriteFile(bin, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, script, "link_tool")
	fn = strings.ReplaceAll(fn, "/usr/local/bin/urnet-tools", filepath.Join(dir, "tool-link"))
	setup := `priv() { return 1; }
pr_err() { :; }
pr_warn() { :; }
` + fn + "\nlink_tool || exit 1\nprintf new > \"$provider_bin\"\n"
	cmd := exec.Command("/bin/sh", "-c", setup)
	cmd.Env = append(os.Environ(), "provider_bin="+bin, "install_path="+dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected link failure")
	}
	got, err := os.ReadFile(bin)
	if err != nil || string(got) != "old" {
		t.Fatalf("provider changed after link failure: %q, %v", got, err)
	}
}

func TestOpenBSDInstallServicePreservesAccount(t *testing.T) {
	skipWithoutPOSIXShell(t)
	data, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, string(data), "do_install_service")
	for _, tc := range []struct{ name, existing, want string }{{"existing", "alice", "alice"}, {"new", "", "bob"}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rc := filepath.Join(dir, "service")
			if tc.existing != "" {
				if err := os.WriteFile(rc, []byte("daemon_user=\""+tc.existing+"\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := `id() { printf '0\n'; }
resolve_service_user() { printf 'bob\n'; }
user_home() { printf '%s\n' "$test_home"; }
write_rc_script() { printf '%s\n' "$1"; }
rcctl() { :; }
pr_info() { :; }
pr_err() { :; }
` + fn + "\ndo_install_service\n"
			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = append(os.Environ(), "rc_script="+rc, "service_name=urnetwork", "test_home="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("got %q, err %v; want %q", out, err, tc.want)
			}
		})
	}
}
