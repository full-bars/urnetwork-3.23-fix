package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxInstallerRollbackReporting(t *testing.T) {
	skipWithoutPOSIXShell(t)
	script, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, init := range []string{"systemd", "openrc"} {
		for _, mode := range []string{"success", "failure", "failure_missing"} {
			t.Run(init+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				bin := filepath.Join(dir, "bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					"urnetwork": "new", "urnetwork.old": "old",
					"rc-update": "#!/bin/sh\nexit 0\n",
					"rc-service": "#!/bin/sh\nprintf 'start\\n' >> \"$TEST_DIR/starts\"\nexit 1\n",
				} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "starts"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				fn := extractShellFunc(t, string(script), "install_"+init+"_units")
				if fn == "" {
					t.Fatal("installer function missing")
				}
				harness := `
pr_err() { printf "$@"; printf '\n'; }
pr_info() { :; }
pr_warn() { :; }
sanitize_restart_dropins() { :; }
openrc_finalize_root_paths() { :; }
openrc_user_home() { printf '%s\n' "$TEST_DIR"; }
openrc_restore_update_entry() { :; }
id() { if [ "$1" = "-u" ]; then printf '0\n'; else return 0; fi; }
systemctl() { if [ "$2" = start ]; then printf 'start\n' >> "$TEST_DIR/starts"; return 1; fi; }
mv() { case "$MOVE_MODE" in failure) return 1;; failure_missing) command rm -f "$install_path/bin/urnetwork"; return 1;; esac; command mv "$@"; }
install_path="$TEST_DIR"
systemd_userdir="$TEST_DIR"
systemd_service="$TEST_DIR/service"
systemd_update_service="$TEST_DIR/update-service"
systemd_update_timer="$TEST_DIR/timer"
systemd_units_stopped=1
openrc_units_stopped=1
openrc_user=testuser
openrc_home="$TEST_DIR"
openrc_initd_file="$TEST_DIR/initd"
` + fn + "\ninstall_" + init + "_units\n"
				cmd := exec.Command("/bin/sh", "-c", harness)
				cmd.Env = append(os.Environ(), "TEST_DIR="+dir, "MOVE_MODE="+mode, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("restart failure must exit nonzero: %s", out)
				}
				text := string(out)
				live, liveErr := os.ReadFile(filepath.Join(bin, "urnetwork"))
				old, oldErr := os.ReadFile(filepath.Join(bin, "urnetwork.old"))
				starts, err := os.ReadFile(filepath.Join(dir, "starts"))
				if err != nil {
					t.Fatal(err)
				}
				if mode != "success" {
					if oldErr != nil || string(old) != "old" || (mode == "failure" && (liveErr != nil || string(live) != "new")) || (mode == "failure_missing" && !os.IsNotExist(liveErr)) {
						t.Errorf("rollback failure disk state: live=%q (%v) old=%q (%v)", live, liveErr, old, oldErr)
					}
					if mode == "failure" && (!strings.Contains(text, "backup remains at") || !strings.Contains(text, "replacement at")) {
						t.Errorf("missing remaining file details: %s", text)
					}
					if mode == "failure_missing" && (!strings.Contains(text, "check backup at") || strings.Contains(text, "backup remains at")) {
						t.Errorf("wrong output for missing binary: %s", text)
					}
					if !strings.Contains(text, "Failed to restore") || !strings.Contains(text, filepath.Join(bin, "urnetwork.old")) || !strings.Contains(text, filepath.Join(bin, "urnetwork")) || strings.Contains(text, "Restored previous binary") {
						t.Errorf("misleading rollback failure output: %s", text)
					}
					if strings.Count(string(starts), "start\n") != 1 {
						t.Errorf("started service again after failed restore: %q", starts)
					}
				} else {
					if string(live) != "old" || !os.IsNotExist(oldErr) {
						t.Errorf("rollback success disk state: live=%q old=%q err=%v", live, old, oldErr)
					}
					if !strings.Contains(text, "Restored previous binary") || strings.Contains(text, "Failed to restore") {
						t.Errorf("wrong rollback success output: %s", text)
					}
					if strings.Count(string(starts), "start\n") != 2 {
						t.Errorf("did not retry service after successful restore: %q", starts)
					}
				}
			})
		}
	}
}
