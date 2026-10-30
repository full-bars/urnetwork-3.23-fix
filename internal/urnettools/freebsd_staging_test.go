package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	for _, uid := range []string{"0", "1000"} {
		for _, tc := range []struct {
			name, digest, gate string
			success            bool
		}{
			{"gate failure", "match", "fail", false},
			{"digest mismatch", "mismatch", "pass", false},
			{"digest missing", "missing", "pass", false},
			{"verified asset", "match", "pass", true},
		} {
			t.Run("uid="+uid+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				tree := filepath.Join(dir, "tree")
				if err := os.MkdirAll(filepath.Join(tree, "bin"), 0755); err != nil {
					t.Fatal(err)
				}
				for name, contents := range map[string]string{"bin/urnetwork": "old provider", "bin/urnet-tools": "old tool", "version": "old version"} {
					if err := os.WriteFile(filepath.Join(tree, name), []byte(contents), 0777); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(filepath.Join(tree, name), 0777); err != nil {
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
id() { if [ "$1" = -u ]; then printf '%s\n' "$TEST_UID"; else command id "$@"; fi; }
priv() {
    if [ "$1" = mv ]; then
        case "$4" in "$provider_bin"|"$install_path/bin/urnet-tools")
            [ -f "$install_path/gate-called" ] || { printf 'live move before gate\n' >&2; exit 43; }
            [ "$DIGEST" = match ] || { printf 'live move before digest verification\n' >&2; exit 44; } ;;
        esac
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
pr_err() { printf 'ERR: '; printf "$@"; printf '\n'; }
http_fetch() {
    if [ "$#" = 1 ]; then printf '{}'; else printf 'new tool' > "$2"; fi
}
jq() { case "$*" in *tag_name*) printf 'test-tag\n' ;; *) [ "$DIGEST" = missing ] || printf '%064d\n' 0 | tr 0 a ;; esac; }
release_lists_asset() { return 0; }
get_asset_digest_from_api_response() {
    case "$2" in urnet-tools-*) [ "$DIGEST" = missing ] || printf '%064d\n' 0 | tr 0 a ;; *) printf '%064d\n' 0 | tr 0 a ;; esac
}
verify_sha256_file() { [ "$(sha256 -q "$1")" = "$2" ]; }
tar() { printf 'new provider' > "$4/provider"; }
uname() { printf 'amd64\n'; }
sha256() { case "$2" in *urnet-tools*) [ "$DIGEST" = mismatch ] && printf '%064d\n' 0 | tr 0 b || printf '%064d\n' 0 | tr 0 a ;; *) printf '%064d\n' 0 | tr 0 a ;; esac; }
enforce_install_tree_perms() {
    [ "$(cat "$provider_bin")" = 'old provider' ] && [ "$(cat "$install_path/bin/urnet-tools")" = 'old tool' ] || { printf 'live file changed before gate\n' >&2; return 1; }
    printf 'gate\n' > "$install_path/gate-called"
    chmod 755 "$provider_bin" "$install_path/bin/urnet-tools"
    if [ "$GATE" = fail ]; then pr_err 'gate failed'; return 1; fi
}
` + privRM + "\n" + cleanup + "\n" + doInstall + "\ndo_install test-tag\n"
				h := filepath.Join(dir, "harness.sh")
				if err := os.WriteFile(h, []byte(harness), 0755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("/bin/sh", h, tree)
				cmd.Env = append(os.Environ(), "DIGEST="+tc.digest, "GATE="+tc.gate, "TEST_UID="+uid, "TMPDIR="+dir)
				out, runErr := cmd.CombinedOutput()
				if (runErr == nil) != tc.success {
					t.Fatalf("success=%v, want %v: %v\n%s", runErr == nil, tc.success, runErr, out)
				}
				read := func(path string) string {
					b, err := os.ReadFile(filepath.Join(tree, path))
					if err != nil {
						t.Fatal(err)
					}
					return string(b)
				}
				provider, tool := "old provider", "old tool"
				if tc.success {
					provider, tool = "new provider", "new tool"
				}
				if p, u := read("bin/urnetwork"), read("bin/urnet-tools"); p != provider || u != tool {
					t.Fatalf("live files = %q / %q, want %q / %q; %s", p, u, provider, tool, out)
				}
				if !tc.success && read("version") != "old version" {
					t.Fatal("failure changed version")
				}
				if !tc.success && !strings.Contains(string(out), "ERR:") {
					t.Fatalf("failure missing error: %s", out)
				}
				if tc.gate == "fail" && tc.digest == "match" {
					if _, err := os.Stat(filepath.Join(tree, "gate-called")); err != nil {
						t.Fatal("gate did not run")
					}
					for _, p := range []string{"bin/urnetwork", "bin/urnet-tools"} {
						fi, err := os.Stat(filepath.Join(tree, p))
						if err != nil || fi.Mode().Perm() != 0755 {
							t.Fatalf("repair not retained for %s: %v %v", p, fi, err)
						}
					}
				}
				entries, err := os.ReadDir(filepath.Join(tree, "bin"))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					// This harness stops immediately after the two renames, before
					// the normal post-service backup cleanup can run.
					if tc.success && strings.HasPrefix(entry.Name(), ".urnetwork.bak.") {
						continue
					}
					if strings.HasPrefix(entry.Name(), ".urnet") {
						t.Errorf("staged file left behind: %s", entry.Name())
					}
				}
			})
		}
	}
}
