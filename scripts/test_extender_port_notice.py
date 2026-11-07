#!/usr/bin/env python3
"""Exercise the extracted install-time extender notice, never an installer or entrypoint.

The notice only prints. It never asks a question and never fails the install: the
extender is on by default, so the installer's job is to say what will happen.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
INSTALLER = "scripts/Provider_Install_Linux.sh"


def helper():
    text = (ROOT / INSTALLER).read_text()
    start = text.index("report_extender_port ()\n(\n")
    end = text.index("\n)\n", start) + 3
    return text[start:end]


class ExtenderPortNoticeTest(unittest.TestCase):
    def run_notice(self, ss_output="", ss_present=True, uid="1000", cap_state="cap_net_bind_service=ep", platform="Linux"):
        with tempfile.TemporaryDirectory() as tmp:
            mock = Path(tmp) / "bin"
            mock.mkdir()
            for tool in ("sh", "sed", "head", "awk", "cat"):
                executable = shutil.which(tool)
                if executable is None:
                    raise RuntimeError(f"missing test dependency: {tool}")
                (mock / tool).symlink_to(executable)
            if ss_present:
                ss = mock / "ss"
                ss.write_text("#!/bin/sh\nprintf '%s' \"$SS_OUTPUT\"\n")
                ss.chmod(0o755)
            binary = Path(tmp) / "urnetwork"
            binary.touch()
            env = dict(os.environ, PATH=str(mock), SS_OUTPUT=ss_output, UID_VALUE=uid,
                       CAP_STATE=cap_state, PLATFORM=platform, BINARY=str(binary))
            harness = r'''
set -eu
uname() { printf '%s\n' "$PLATFORM"; }
id() { printf '%s\n' "$UID_VALUE"; }
getcap() { printf '%s %s\n' "$1" "$CAP_STATE"; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
''' + helper() + '\nreport_extender_port "$BINARY"\n'
            result = subprocess.run(["sh", "-c", harness], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout

    def test_free_port_says_the_extender_will_be_on_and_how_to_disable_it(self):
        out = self.run_notice(ss_output="")
        self.assertIn("INFO:", out)
        self.assertIn("443 is free", out)
        self.assertIn("on by default", out)
        self.assertIn("urnet-tools extender disable", out)
        self.assertNotIn("WARN:", out)

    def test_taken_port_names_the_holder_and_says_it_will_keep_retrying(self):
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("caddy",pid=894,fd=7))\n')
        self.assertIn("WARN:", out)
        self.assertIn("caddy (pid 894)", out)
        self.assertIn("keep retrying", out)
        self.assertIn("urnet-tools extender disable", out)

    def test_taken_by_a_process_whose_name_is_hidden_says_how_to_find_it(self):
        out = self.run_notice(ss_output="LISTEN 0 4096 *:443 *:*\n")
        self.assertIn("WARN:", out)
        self.assertIn("sudo ss -ltnp", out)

    def test_an_update_does_not_blame_the_providers_own_listener(self):
        out = self.run_notice(ss_output='LISTEN 0 4096 *:443 *:* users:(("urnetwork",pid=10,fd=9))\n')
        self.assertNotIn("WARN:", out)
        self.assertIn("already", out)

    def test_missing_bind_capability_is_said_plainly_for_a_non_root_user(self):
        out = self.run_notice(ss_output="", uid="1000", cap_state="")
        self.assertIn("WARN:", out)
        self.assertIn("setcap cap_net_bind_service=+ep", out)

    def test_root_needs_no_capability(self):
        out = self.run_notice(ss_output="", uid="0", cap_state="")
        self.assertNotIn("WARN:", out)

    def test_without_ss_it_still_explains_and_does_not_fail(self):
        out = self.run_notice(ss_present=False)
        self.assertIn("on by default", out)
        self.assertIn("could not check", out)

    def test_other_platforms_print_nothing(self):
        self.assertEqual(self.run_notice(platform="Darwin"), "")


if __name__ == "__main__":
    unittest.main()
