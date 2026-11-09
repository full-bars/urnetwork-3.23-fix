#!/usr/bin/env python3
"""Exercise installer systemd failure paths with shell stubs and temp files."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


class InstallerSystemdFailures(unittest.TestCase):
    def run_install(self, failure, backup=False):
        script = INSTALLER.read_text()
        start = script.index("install_systemd_units ()\n{\n")
        end = script.index("\n}\n", start) + 3
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "bin").mkdir()
            (root / "bin/urnetwork").write_text("new")
            if backup:
                (root / "bin/urnetwork.old").write_text("old")
            harness = r'''
set -eu
systemd_units_stopped=1
systemd_userdir="$ROOT/units"
systemd_service="$systemd_userdir/urnetwork.service"
systemd_update_service="$systemd_userdir/urnetwork-update.service"
systemd_update_timer="$systemd_userdir/urnetwork-update.timer"
install_path="$ROOT"
update_timer_oncalendar=daily
sanitize_restart_dropins() { :; }
ensure_extender_cap() { :; }
id() { printf '1000\n'; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
systemctl() {
    printf '%s\n' "$*" >> "$ROOT/calls"
    case "$FAILURE:$*" in
        service:'--user enable urnetwork.service'|timer:'--user enable --now urnetwork-update.timer'|reload:'--user daemon-reload') printf 'stub failure\n' >&2; return 1 ;;
        start:'--user start urnetwork.service') printf 'stub failure\n' >&2; return 1 ;;
    esac
    return 0
}
''' + script[start:end] + '\ninstall_systemd_units\n'
            result = subprocess.run(["sh", "-c", harness], env=dict(os.environ, ROOT=tmp, FAILURE=failure), text=True, capture_output=True)
            binary = (root / "bin/urnetwork").read_text()
            calls = (root / "calls").read_text()
            return result, binary, calls

    def test_service_enable_failure_explains_partial_state(self):
        result, binary, calls = self.run_install("service")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("service enable failed", result.stdout)
        self.assertIn("stub failure", result.stdout)
        self.assertIn("systemctl --user daemon-reload", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_timer_failure_explains_partial_state(self):
        result, binary, calls = self.run_install("timer")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("timer enable/start did not complete", result.stdout)
        self.assertIn("stub failure", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_reload_failure_explains_partial_state(self):
        result, binary, calls = self.run_install("reload")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("daemon-reload failed", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_failed_restart_reports_failed_rollback_start(self):
        result, binary, calls = self.run_install("start", backup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "old")
        self.assertIn("Restored previous binary", result.stdout)
        self.assertIn("Starting the restored binary failed", result.stdout)
        self.assertNotIn("restored provider service started", result.stdout)
        self.assertEqual(calls.count("--user start urnetwork.service"), 2)


if __name__ == "__main__":
    unittest.main()
