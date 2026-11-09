#!/usr/bin/env python3
"""Exercise installer systemd failure paths with shell stubs and temp files."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


class InstallerSystemdFailures(unittest.TestCase):
    def run_install(self, failure, backup=False, existing_unit=False, stopped=True):
        script = INSTALLER.read_text()
        start = script.index("install_systemd_units ()\n{\n")
        end = script.index("\n}\n", start) + 3
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "bin").mkdir()
            (root / "bin/urnetwork").write_text("new")
            if backup:
                (root / "bin/urnetwork.old").write_text("old")
            if existing_unit:
                (root / "units").mkdir()
                (root / "units/urnetwork.service").write_text("[Service]\nType=simple\n")
            harness = r'''
set -eu
systemd_units_stopped="$STOPPED"
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
        start_once:'--user start urnetwork.service')
            if [ ! -f "$ROOT/started_once" ]; then
                touch "$ROOT/started_once"
                printf 'stub failure\n' >&2
                return 1
            fi
            ;;
    esac
    return 0
}
''' + script[start:end] + '\ninstall_systemd_units prepare\ninstall_systemd_units\n'
            result = subprocess.run(["sh", "-c", harness], env=dict(os.environ, ROOT=tmp, FAILURE=failure, STOPPED=str(int(stopped))), text=True, capture_output=True)
            unit = (root / "units/urnetwork.service").read_text()
            binary = (root / "bin/urnetwork").read_text()
            calls = (root / "calls").read_text()
            return result, binary, calls, unit

    def test_backup_failure_and_stale_backup(self):
        script = INSTALLER.read_text()
        start = script.index("install_systemd_units ()\n{\n")
        end = script.index("\n}\n", start) + 3
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "units").mkdir()
            (root / "bin").mkdir()
            unit = root / "units/urnetwork.service"
            unit.write_text("[Service]\nType=simple\n")
            (root / "units/urnetwork.service.old").write_text("stale")
            (root / "bin/urnetwork").write_text("new")
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
cp() { printf 'simulated copy failure\n' >&2; return 1; }
systemctl() { printf '%s\n' "$*" >> "$ROOT/calls"; return 0; }
''' + script[start:end] + '\ninstall_systemd_units prepare\ninstall_systemd_units\n'
            result = subprocess.run(["sh", "-c", harness], env=dict(os.environ, ROOT=tmp), text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertEqual(unit.read_text(), "[Service]\nType=simple\n")
            self.assertEqual((root / "bin/urnetwork").read_text(), "new")
            self.assertFalse((root / "calls").exists())
            self.assertIn("back", result.stdout.lower())
            self.assertIn("cp ", result.stdout)

    def test_rerun_keeps_original_backup_without_trusting_it(self):
        script = INSTALLER.read_text()
        start = script.index("install_systemd_units ()\n{\n")
        end = script.index("\n}\n", start) + 3
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "units").mkdir()
            (root / "bin").mkdir()
            unit = root / "units/urnetwork.service"
            backup = root / "units/urnetwork.service.old"
            unit.write_text("[Service]\nType=simple\n")
            (root / "bin/urnetwork").write_text("new")
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
    case "$FAILURE:$*" in
        service:'--user enable urnetwork.service'|start:'--user start urnetwork.service') return 1 ;;
    esac
    return 0
}
''' + script[start:end] + '\ninstall_systemd_units prepare\ninstall_systemd_units\n'
            env = dict(os.environ, ROOT=tmp, FAILURE="service")
            first = subprocess.run(["sh", "-c", harness], env=env, text=True, capture_output=True)
            self.assertNotEqual(first.returncode, 0, first.stdout)
            self.assertEqual(backup.read_text(), "[Service]\nType=simple\n")
            self.assertIn("Type=notify", unit.read_text())
            env["FAILURE"] = "start"
            second = subprocess.run(["sh", "-c", harness], env=env, text=True, capture_output=True)
            self.assertNotEqual(second.returncode, 0, second.stdout)
            self.assertEqual(backup.read_text(), "[Service]\nType=simple\n")
            self.assertIn("Type=notify", unit.read_text())
            self.assertEqual((root / "bin/urnetwork").read_text(), "old")
            self.assertIn("No usable systemd unit backup was made by this run", second.stdout)
            self.assertIn("Type=notify unit remains", second.stdout)
            self.assertNotIn("Restored previous binary and systemd unit", second.stdout)

    def test_fresh_and_existing_simple_units_become_notify(self):
        for existing, stopped in ((False, False), (True, True), (True, False)):
            with self.subTest(existing=existing, stopped=stopped):
                result, _, _, unit = self.run_install("", existing_unit=existing, stopped=stopped)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("\nType=notify\nNotifyAccess=all\n", unit)
                self.assertNotIn("Type=simple", unit)
                self.assertNotIn("TimeoutStartSec=", unit)

    def test_service_enable_failure_explains_partial_state(self):
        result, binary, calls, _ = self.run_install("service")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("service enable failed", result.stdout)
        self.assertIn("stub failure", result.stdout)
        self.assertIn("systemctl --user daemon-reload", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_timer_failure_explains_partial_state(self):
        result, binary, calls, _ = self.run_install("timer")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("timer enable/start did not complete", result.stdout)
        self.assertIn("stub failure", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_reload_failure_explains_partial_state(self):
        result, binary, calls, _ = self.run_install("reload")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "new")
        self.assertIn("daemon-reload failed", result.stdout)
        self.assertNotIn("--user start", calls)

    def test_failed_restart_reports_failed_rollback_start(self):
        result, binary, calls, _ = self.run_install("start", backup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "old")
        self.assertIn("Restored previous binary", result.stdout)
        self.assertIn("Starting the restored binary failed", result.stdout)
        self.assertNotIn("restored provider service started", result.stdout)
        self.assertEqual(calls.count("--user start urnetwork.service"), 2)

    def test_rollback_restores_prior_simple_unit_and_reloads(self):
        result, binary, calls, unit = self.run_install("start_once", backup=True, existing_unit=True, stopped=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "old")
        self.assertIn("Type=simple", unit)
        self.assertNotIn("Type=notify", unit)
        self.assertIn("Restored previous binary and systemd unit after service restart failure", result.stdout)
        self.assertIn("The restored provider service started.", result.stdout)
        self.assertEqual(calls.count("--user daemon-reload"), 2)
        self.assertEqual(calls.count("--user start urnetwork.service"), 2)

    def test_failed_rollback_with_existing_simple_unit_keeps_restored_unit(self):
        result, binary, calls, unit = self.run_install("start", backup=True, existing_unit=True, stopped=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(binary, "old")
        self.assertIn("Type=simple", unit)
        self.assertNotIn("Type=notify", unit)
        self.assertIn("Restored previous binary and systemd unit after service restart failure", result.stdout)
        self.assertIn("Starting the restored binary failed", result.stdout)
        self.assertEqual(calls.count("--user daemon-reload"), 2)
        self.assertEqual(calls.count("--user start urnetwork.service"), 2)


if __name__ == "__main__":
    unittest.main()
