#!/usr/bin/env python3
"""Exercise privileged installer helpers with shell stubs."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


def function(name):
    script = INSTALLER.read_text()
    marker = name + ("() {\n" if name == "firewall_hint" else " ()\n{\n")
    start = script.index(marker)
    if name == "install_openrc_units":
        return script[start:script.index("\n# Run a command string as another user", start)]
    if name == "openrc_finalize_root_paths":
        return script[start:script.index("\n# install_openrc_units:", start)]
    return script[start:script.index("\n}\n", start) + 3]


class PrivilegedSteps(unittest.TestCase):
    def test_optimize_dispatch_preserves_approval_through_sudo(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, content in {
                "id": "#!/bin/sh\nprintf '1000\\n'\n",
                "sudo": "#!/bin/sh\nprintf 'SUDO:%s\\n' \"$*\"\nexit 73\n",
            }.items():
                stub = root / name
                stub.write_text(content)
                stub.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = tmp + os.pathsep + env["PATH"]
            for args in (("-y", "optimize"), ("optimize", "-y"),
                         ("--force", "optimize"), ("optimize", "--yes")):
                with self.subTest(args=args):
                    result = subprocess.run(["sh", str(INSTALLER), *args],
                                            env=env, text=True, capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"SUDO:-n {INSTALLER} -y optimize", result.stdout)

    def test_restart_confirmation_does_not_prompt_on_pipe(self):
        harness = '''
FORCE=0
pr_err() { printf '%s\\n' "$*"; }
''' + function("confirm_restart") + '\nconfirm_restart test\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("without a terminal", result.stdout)
        self.assertNotIn("Are you absolutely sure", result.stdout)

    def test_linger_failure_prints_error_and_recovery(self):
        harness = '''
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
loginctl() { printf 'stub loginctl failure\\n' >&2; return 1; }
''' + function("installer_enable_linger") + '\ninstaller_enable_linger alice\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stub loginctl failure", result.stderr)
        self.assertIn("sudo loginctl enable-linger alice", result.stdout)
        self.assertIn("may stop on logout", result.stdout)

    def test_tool_copy_failure_preserves_backup_and_reports_state(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "bin").mkdir()
            (root / "bin/urnet-tools").write_text("old")
            source = root / "download"
            source.write_text("new")
            harness = '''
install_path="$1"
pr_err() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
cp() { printf 'copy denied\\n' >&2; return 1; }
''' + function("install_downloaded_tool") + '\ninstall_downloaded_tool "$1/download"\n'
            result = subprocess.run(["sh", "-c", harness, "sh", tmp], text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((root / "bin/urnet-tools.old").read_text(), "old")
            self.assertTrue(source.exists())
            self.assertIn("copy denied", result.stderr)
            self.assertIn("provider binary is installed", result.stdout)
            self.assertIn("service may be stopped", result.stdout)
            self.assertIn("sudo cp", result.stdout)

    def test_firewall_queries_expose_failure_and_recovery(self):
        for name, expected in (("firewall-cmd", "sudo firewall-cmd --state"), ("nft", "sudo nft list tables")):
            with self.subTest(name=name):
                with tempfile.TemporaryDirectory() as tmp:
                    stub = Path(tmp) / name
                    stub.write_text("#!/bin/sh\nprintf 'query denied\\n' >&2\nexit 1\n")
                    stub.chmod(0o755)
                    harness = function("firewall_hint") + '\nfirewall_hint 443 tcp\n'
                    result = subprocess.run(["/bin/sh", "-c", harness], env={"PATH": tmp}, text=True, capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("query denied", result.stderr)
                    self.assertIn(expected, result.stderr)

    def test_sysctl_config_failure_does_not_claim_success(self):
        harness = '''
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
pr_info() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
sysctl() { printf 'failed %s\\n' "$*" >&2; return 1; }
''' + function("installer_apply_sysctl_config") + '\ninstaller_apply_sysctl_config /etc/sysctl.d/99-urnetwork.conf\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("failed -p", result.stderr)
        self.assertIn("failed --system", result.stderr)
        self.assertIn("sudo sysctl -p '/etc/sysctl.d/99-urnetwork.conf'", result.stdout)
        self.assertNotIn("settings applied", result.stdout)

    def test_immediate_sysctls_report_each_failure_and_continue(self):
        harness = '''
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
sysctl() { printf 'denied %s\\n' "$2" >&2; return 1; }
''' + function("installer_apply_immediate_sysctls") + '\ninstaller_apply_immediate_sysctls\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0)
        for setting in ("fs.inotify.max_user_watches=524288", "fs.inotify.max_user_instances=512", "fs.file-max=2097152"):
            self.assertIn("sudo sysctl -w " + setting, result.stdout)
            self.assertIn("denied " + setting, result.stderr)

    def test_openrc_security_failure_prevents_service_install(self):
        for root_mode in (True, False):
            with self.subTest(root=root_mode), tempfile.TemporaryDirectory() as tmp:
                harness = '''
install_path="$1"; openrc_install_root="$1"; openrc_user=alice; openrc_initd_file="$1/init"
openrc_user_home() { printf '%s\\n' "$install_path"; }
openrc_restore_update_entry() { :; }
openrc_finalize_root_paths() { printf 'security denied\\n' >&2; return 1; }
id() { if [ "$1" = -u ]; then printf '%s\\n' "$ROOT_ID"; fi; return 0; }
chmod() { :; }
sudo() { if [ "$1" = -n ]; then return 0; fi; printf 'unexpected sudo %s\\n' "$*" >&2; return 1; }
cp() { printf 'unexpected install\\n' >&2; return 1; }
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
pr_err() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
pr_info() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
''' + function("install_openrc_units") + '\ninstall_openrc_units\n'
                result = subprocess.run(["sh", "-c", harness, "sh", tmp],
                                        env={"ROOT_ID": "0" if root_mode else "1000"},
                                        text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("security denied", result.stderr)
                self.assertNotIn("unexpected", result.stderr)
                self.assertIn("service may be stopped", result.stdout)
                self.assertNotIn("rc-service urnetwork start", result.stdout)

    def test_openrc_security_operations_fail_closed(self):
        for failed_step in ("find", "chown", "chmod"):
            with self.subTest(step=failed_step):
                harness = '''
install_path=/stub; openrc_home=/stub; openrc_user=alice
pr_err() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
find() { [ "$FAIL" != find ]; }
chown() { [ "$FAIL" != chown ]; }
chmod() { [ "$FAIL" != chmod ]; }
mkdir() { :; }; touch() { :; }; tee() { :; }
openrc_stage_root_owned_tool() { :; }
pr_info() { :; }; pr_warn() { :; }
''' + function("openrc_finalize_root_paths") + '\nopenrc_finalize_root_paths\n'
                result = subprocess.run(["sh", "-c", harness], env={"FAIL": failed_step}, text=True, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("sudo " + failed_step, result.stdout)

    def test_openrc_staging_chown_failure_never_reports_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "bin").mkdir()
            (root / "bin/urnet-tools").write_text("stub")
            harness = '''
install_path="$1"; openrc_root_tool_dir="$1/stage"; openrc_root_tool_path="$1/stage/urnet-tools"
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
id() { printf '0\\n'; }
mkdir() { :; }
chown() { printf 'chown denied\\n' >&2; return 1; }
cp() { printf 'unexpected copy\\n' >&2; return 1; }
''' + function("openrc_stage_root_owned_tool") + '\nopenrc_stage_root_owned_tool\n'
            result = subprocess.run(["sh", "-c", harness, "sh", tmp], text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("chown denied", result.stderr)
            self.assertNotIn("unexpected copy", result.stderr)
            self.assertIn("sudo chown root:root", result.stdout)

    def test_openrc_log_chown_failure_prints_recovery(self):
        harness = '''
install_path=/stub; openrc_home=/stub; openrc_user=alice
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
pr_err() { printf "$@\\n"; }
pr_info() { printf "$@\\n"; }
find() { :; }; mkdir() { :; }; chmod() { :; }; touch() { :; }; tee() { :; }
chown() { case "$*" in *urnetwork.log*) printf 'log denied\\n' >&2; return 1 ;; esac; }
openrc_stage_root_owned_tool() { return 1; }
''' + function("openrc_finalize_root_paths") + '\nopenrc_finalize_root_paths\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertIn("log denied", result.stderr)
        self.assertIn("sudo chown 'alice' /var/log/urnetwork.log /var/log/urnetwork.err", result.stdout)
        self.assertNotIn("Staged the auto-update tool", result.stdout)

    def test_linger_success_is_silent(self):
        harness = '''
pr_warn() { fmt="$1"; shift; printf "$fmt\\n" "$@"; }
loginctl() { return 0; }
''' + function("installer_enable_linger") + '\ninstaller_enable_linger alice\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout + result.stderr, "")


if __name__ == "__main__":
    unittest.main()
