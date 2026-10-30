package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual installer and cleanup with local release stubs. The gate
// checks the live files itself so an early rename cannot satisfy the test.
func TestFreeBSDToolAndProviderStagingOrder(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	doInstall := extractShellFunc(t, script, "do_install")
	cleanup := extractShellFunc(t, script, "install_cleanup")
	privRM := extractShellFunc(t, script, "priv_rm")
	if doInstall == "" || cleanup == "" || privRM == "" {
		t.Fatal("missing installer functions")
	}
	for _, tc := range []struct {
		name, digest, gate string
		wantSuccess        bool
	}{
		{"gate failure", "match", "fail", false},
		{"digest mismatch", "mismatch", "pass", false},
		{"verified asset", "match", "pass", true},
		{"digest missing", "missing", "pass", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tree := filepath.Join(dir, "tree")
			if err := os.MkdirAll(filepath.Join(tree, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{"bin/urnetwork": "old provider", "bin/urnet-tools": "old tool", "version": "old version"} {
				if err := os.WriteFile(filepath.Join(tree, name), []byte(contents), 0755); err != nil {
					t.Fatal(err)
				}
			}
			harness := `install_path="$1"
provider_bin="$install_path/bin/urnetwork"
state_dir="$install_path/state"
service_user=testuser
github_api=example
github_raw=example
me=test
staged_bin= staged_tool= staged_version= tmpdir=
id() { if [ "$1" = -u ]; then printf '1000\n'; else command id "$@"; fi; }
priv() {
    if [ "$1" = mv ] && [ "$4" = "$install_path/bin/urnet-tools" ]; then
        [ -f "$install_path/gate-called" ] || printf 'tool before gate\n' >&2
    fi
    if [ "$1" = mv ] && [ "$4" = "$provider_bin" ]; then
        [ -f "$install_path/gate-called" ] || { printf 'provider before gate\n' >&2; exit 43; }
    fi
    "$@"
    result=$?
    if [ "$1" = mv ] && [ "$result" = 0 ]; then
        case "$4" in "$provider_bin"|"$install_path/bin/urnet-tools")
            printf 'move\n' >> "$install_path/moves"
            if [ "$(wc -l < "$install_path/moves" | tr -d ' ')" = 2 ]; then exit 0; fi ;;
        esac
    fi
    return "$result"
}
assert_state_path_safe() { :; }
pr_info() { :; }
pr_warn() { :; }
pr_err() { :; }
http_fetch() {
    if [ "$#" = 1 ]; then
        if [ "$DIGEST" = missing ]; then printf '{"assets":[]}'; else printf '{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}'; fi
    else
        case "$1" in *Provider_Install_FreeBSD.sh) printf 'new wrapper' > "$2" ;; *) printf 'new tool' > "$2" ;; esac
    fi
}
jq() { case "$*" in *tag_name*) printf 'test-tag\n' ;; *) [ "$DIGEST" = missing ] || printf '%064d\n' 0 | tr 0 a ;; esac; }
tar() { printf 'new provider' > "$4/provider"; }
uname() { printf 'amd64\n'; }
sha256() { if [ "$DIGEST" = match ]; then printf '%064d\n' 0 | tr 0 a; else printf '%064d\n' 0 | tr 0 b; fi; }
enforce_install_tree_perms() {
    [ "$(cat "$provider_bin")" = 'old provider' ] && [ "$(cat "$install_path/bin/urnet-tools")" = 'old tool' ] || { printf 'live file changed before gate\n' >&2; return 1; }
    printf 'gate\n' > "$install_path/gate-called"
    [ "$GATE" = pass ]
}
` + privRM + "\n" + cleanup + "\n" + doInstall + "\ndo_install test-tag\n"
			h := filepath.Join(dir, "harness.sh")
			if err := os.WriteFile(h, []byte(harness), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", h, tree)
			cmd.Env = append(os.Environ(), "DIGEST="+tc.digest, "GATE="+tc.gate, "TMPDIR="+dir)
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.wantSuccess {
				t.Fatalf("success=%v, want %v: %v\n%s", runErr == nil, tc.wantSuccess, runErr, out)
			}
			read := func(path string) string {
				b, err := os.ReadFile(filepath.Join(tree, path))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			provider, tool := "old provider", "old tool"
			if tc.wantSuccess {
				provider = "new provider"
				tool = "new tool"
			}
			if p, u := read("bin/urnetwork"), read("bin/urnet-tools"); p != provider || u != tool {
				t.Fatalf("live files = %q / %q, want %q / %q; %s", p, u, provider, tool, out)
			}
			if !tc.wantSuccess && read("version") != "old version" {
				t.Fatal("failure changed version")
			}
			entries, err := os.ReadDir(filepath.Join(tree, "bin"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".urnet") {
					t.Errorf("staged file left behind: %s", entry.Name())
				}
			}
		})
	}
}
