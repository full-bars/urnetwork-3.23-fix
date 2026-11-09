#!/usr/bin/env python3
"""Run the real install flow with an injected systemd unit backup failure."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


def function(script, name):
    start = script.index(name + " ()\n{\n")
    end = script.index("\n}\n", start) + 3
    return script[start:end]


class UnitBackupEndToEnd(unittest.TestCase):
    def test_failed_backup_does_not_replace_binary_or_unit(self):
        script = INSTALLER.read_text()
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "install/bin").mkdir(parents=True)
            (root / "units").mkdir()
            binary = root / "install/bin/urnetwork"
            unit = root / "units/urnetwork.service"
            binary.write_text("old binary")
            unit.write_text("[Service]\nType=simple\n")
            (root / "units/urnetwork.service.old").write_text("stale backup")
            harness = r'''
install_path="$ROOT/install"
systemd_userdir="$ROOT/units"
systemd_service="$systemd_userdir/urnetwork.service"
systemd_update_service="$systemd_userdir/urnetwork-update.service"
systemd_update_timer="$systemd_userdir/urnetwork-update.timer"
script_rundir="$ROOT"
tag=vtest
operation=update
has_systemd=1
has_openrc=0
arch=amd64
api_base=mock
urnet_dl_base=mock
urnet_github_dl_base=mock
URNETWORK_NO_DOWNLOAD_TARBALL=''
update_timer_oncalendar=daily
pr_info() { fmt="$1"; shift; printf "INFO: $fmt\n" "$@"; }
pr_warn() { :; }
pr_err() { fmt="$1"; shift; printf "ERROR: $fmt\n" "$@"; }
openrc_maybe_redirect_install_path() { :; }
install_downloader() { :; }
install_json_parser() { :; }
func_root_guard() { :; }
get_version_from_api_response() { printf vtest; }
get_release_date_from_api_response() { printf 1; }
network_fetch() { printf '{}'; }
release_lists_asset() { return 0; }
get_asset_digest_from_api_response() { :; }
download_asset() { printf payload > "$2"; }
tar() { mkdir -p "$workdir/linux/amd64"; printf 'new binary' > "$workdir/linux/amd64/provider"; }
systemctl() { printf '%s\n' "$*" >> "$ROOT/systemctl-calls"; }
stop_systemd_units() { systemctl --user stop urnetwork.service; systemd_units_stopped=1; }
sanitize_restart_dropins() { :; }
ensure_extender_cap() { :; }
report_extender_port() { :; }
offer_extender_capability() { :; }
cp() {
    case "$*" in *"$systemd_service "*"$systemd_service.backup."*) printf 'injected unit copy failure\n' >&2; return 1;; esac
    command cp "$@"
}
''' + function(script, "install_systemd_units") + "\n" + function(script, "do_install") + "\ndo_install\n"
            result = subprocess.run(["sh", "-c", harness], env=dict(os.environ, ROOT=tmp), text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("Could not back up the previous systemd unit", result.stdout)
            self.assertIn("cp -p '" + str(unit) + "' '" + str(unit) + ".old'", result.stdout)
            self.assertEqual(binary.read_text(), "old binary", result.stdout + result.stderr)
            self.assertEqual(unit.read_text(), "[Service]\nType=simple\n")
            self.assertFalse((root / "systemctl-calls").exists())
            self.assertEqual((root / "units/urnetwork.service.old").read_text(), "stale backup")


if __name__ == "__main__":
    unittest.main()
