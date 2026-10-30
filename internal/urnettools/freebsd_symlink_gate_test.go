package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreeBSDInstallTreePermsRejectsSymlinks(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "enforce_install_tree_perms")
	if fn == "" {
		t.Fatal("missing enforce_install_tree_perms")
	}
	harness := `priv() { if [ "${FAIL_SCAN:-0}" = 1 ] && [ "$1" = find ] && [ "$3" = -type ] && [ "$4" = l ]; then return 1; fi; "$@"; }
pr_err() { fmt="$1"; shift; printf 'ERR: '"$fmt"'\n' "$@" >&2; }
pr_info() { :; }
stat() { case "$2" in %u) printf '0\n' ;; %Lp) printf '755\n' ;; *) return 1 ;; esac; }
chown() { printf 'chown\n' >> "$REPAIRS"; [ "${FAIL_CHOWN:-0}" != 1 ]; }
chmod() { printf 'chmod\n' >> "$REPAIRS"; }
` + fn + `
enforce_install_tree_perms "$TREE" "$BIN"
`
	for _, tc := range []struct {
		name, target                 string
		failChown, failScan, success bool
	}{
		{"file", "urnetwork", false, false, false},
		{"directory", ".", false, false, false},
		{"dangling", "missing", false, false, false},
		{"already conformed fallback", "urnetwork", true, false, false},
		{"scan failure", "", false, true, false},
		{"no link", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := filepath.Join(t.TempDir(), "tree")
			binDir := filepath.Join(tree, "bin")
			if err := os.MkdirAll(binDir, 0755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(binDir, "urnetwork")
			if err := os.WriteFile(bin, []byte("binary"), 0755); err != nil {
				t.Fatal(err)
			}
			wantPath := tree
			if tc.target != "" {
				wantPath = filepath.Join(binDir, "link with spaces")
				if err := os.Symlink(tc.target, wantPath); err != nil {
					t.Fatal(err)
				}
			}
			repairs := filepath.Join(t.TempDir(), "repairs")
			cmd := exec.Command("/bin/sh", "-c", harness)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TREE=" + tree, "BIN=" + bin, "REPAIRS=" + repairs}
			if tc.failChown {
				cmd.Env = append(cmd.Env, "FAIL_CHOWN=1")
			}
			if tc.failScan {
				cmd.Env = append(cmd.Env, "FAIL_SCAN=1")
			}
			out, err := cmd.CombinedOutput()
			if tc.success {
				if err != nil {
					t.Fatalf("gate rejected a safe tree: %v: %s", err, out)
				}
				if _, err := os.Stat(repairs); err != nil {
					t.Fatalf("repair did not run: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("gate accepted %s: %s", wantPath, out)
			}
			if !strings.Contains(string(out), wantPath) || !strings.Contains(string(out), "ERR:") {
				t.Errorf("error must name %s: %s", wantPath, out)
			}
			if _, err := os.Stat(repairs); !os.IsNotExist(err) {
				t.Errorf("repair ran before rejection: %v", err)
			}
		})
	}
}
