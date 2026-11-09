#!/usr/bin/env python3
"""Exercise privileged installer helpers with shell stubs."""
from pathlib import Path
import subprocess
import unittest

INSTALLER = Path(__file__).resolve().parent / "Provider_Install_Linux.sh"


def function(name):
    script = INSTALLER.read_text()
    start = script.index(name + " ()\n{\n")
    return script[start:script.index("\n}\n", start) + 3]


class PrivilegedSteps(unittest.TestCase):
    def test_linger_failure_prints_error_and_recovery(self):
        harness = '''
pr_warn() { printf "$@\\n"; }
loginctl() { printf 'stub loginctl failure\\n' >&2; return 1; }
''' + function("installer_enable_linger") + '\ninstaller_enable_linger alice\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("stub loginctl failure", result.stderr)
        self.assertIn("sudo loginctl enable-linger alice", result.stdout)
        self.assertIn("may stop on logout", result.stdout)

    def test_linger_success_is_silent(self):
        harness = '''
pr_warn() { printf "$@\\n"; }
loginctl() { return 0; }
''' + function("installer_enable_linger") + '\ninstaller_enable_linger alice\n'
        result = subprocess.run(["sh", "-c", harness], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout + result.stderr, "")


if __name__ == "__main__":
    unittest.main()
