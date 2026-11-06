#!/usr/bin/env python3
"""Exercise the extracted capability helper, never an installer or entrypoint."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPTS = (
    "scripts/Provider_Install_Linux.sh",
    "docker/scripts/provider_common.sh",
    "docker/scripts/urnet-tools.sh",
)


def helper(path):
    text = (ROOT / path).read_text()
    start = text.index("ensure_extender_cap ()\n(\n")
    end = text.index("\n)\n", start) + 3
    return text[start:end]


class ExtenderCapabilityTest(unittest.TestCase):
    def run_helper(self, script, initial, mode="success", platform="Linux", capbnd=None):
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp) / "state"
            calls = Path(tmp) / "calls"
            binary = Path(tmp) / "fake provider's binary"
            binary.touch()
            state.write_text(initial)
            status = Path(tmp) / "status"
            if capbnd is not None:
                status.write_text("Name:\tsh\nCapInh:\t0000000000000000\nCapBnd:\t" + capbnd + "\nCapAmb:\t0000000000000000\n")
            mock_path = Path(tmp) / "bin"
            mock_path.mkdir()
            for tool in ("sh", "awk", "cat", "sed"):
                executable = shutil.which(tool)
                if executable is None:
                    raise RuntimeError(f"missing test dependency: {tool}")
                (mock_path / tool).symlink_to(executable)
            env = dict(os.environ, STATE=str(state), CALLS=str(calls), PATH=str(mock_path),
                       MODE=mode, PLATFORM=platform, BINARY=str(binary), SUDO="")
            harness = r'''
set -eu
uname() { printf '%s\n' "$PLATFORM"; }
getcap() { printf '%s %s\n' "$1" "$(cat "$STATE")"; }
setcap() {
    printf '%s\n' "$*" >> "$CALLS"
    [ "$MODE" != fail ] || return 1
    [ "$MODE" != unverified ] || return 0
    if [ "$1" = -r ]; then printf '%s' '' > "$STATE"; else printf '%s' 'cap_net_bind_service=ep' > "$STATE"; fi
}
[ "$MODE" != missing-setcap ] || unset -f setcap
[ "$MODE" != missing-getcap ] || unset -f getcap
''' + helper(script).replace("/proc/self/status", str(status)) + '\nensure_extender_cap "$BINARY"\n'
            result = subprocess.run(["/bin/sh", "-c", harness], env=env,
                                    text=True, capture_output=True, check=False)
            return result, calls.read_text() if calls.exists() else "", str(binary)

    def test_scenarios(self):
        for script in SCRIPTS:
            for initial, mode, platform, expect_call, warn in (
                ("", "success", "Linux", True, False),
                ("cap_net_bind_service=ep", "success", "Linux", False, False),
                ("cap_chown,cap_net_bind_service=ep", "success", "Linux", False, False),
                ("cap_net_bind_service=p", "success", "Linux", True, False),
                ("", "fail", "Linux", True, True),
                ("", "unverified", "Linux", True, True),
                ("", "missing-setcap", "Linux", False, True),
                ("", "missing-getcap", "Linux", True, True),
                ("", "success", "Darwin", False, False),
            ):
                with self.subTest(script=script, initial=initial, mode=mode, platform=platform):
                    result, calls, binary = self.run_helper(script, initial, mode, platform)
                    self.assertEqual(bool(calls), expect_call)
                    if expect_call:
                        self.assertEqual(calls, f"cap_net_bind_service=+ep {binary}" + chr(10))
                    else:
                        self.assertEqual(calls, "")
                    self.assertEqual("WARNING:" in result.stderr, warn)
                    if warn:
                        self.assertEqual(len(result.stderr.splitlines()), 1)
                        quoted = "'" + binary.replace("'", "'" + chr(92) + "''") + "'"
                        self.assertIn("sudo setcap cap_net_bind_service=+ep " + quoted, result.stderr)

    def test_bounding_set(self):
        for script in SCRIPTS:
            for capbnd, applied in (
                ("000001ffffffffff", True),
                ("00000000a80425fb", True),
                ("00000000a80421fb", False),
                ("000001fffffffbff", False),
            ):
                with self.subTest(script=script, capbnd=capbnd):
                    result, calls, binary = self.run_helper(script, "", "success", "Linux", capbnd)
                    if applied:
                        self.assertEqual(calls, f"cap_net_bind_service=+ep {binary}" + chr(10))
                        self.assertEqual(result.stderr, "")
                    else:
                        self.assertEqual(calls, "")
                        self.assertEqual(len(result.stderr.splitlines()), 1)
                        self.assertIn("bounding set", result.stderr)
                self.assertNotIn("would fail to start", result.stderr)

    def test_existing_capability_outside_bounding_set_warns(self):
        for script in SCRIPTS:
            with self.subTest(script=script):
                result, calls, binary = self.run_helper(script, "cap_net_bind_service=ep", capbnd="00000000a80421fb")
                self.assertEqual(calls, f"-r {binary}\n")
                self.assertIn("bounding set", result.stderr)

    def test_blocked_cap_does_not_remove_other_capabilities(self):
        for script in SCRIPTS:
            with self.subTest(script=script):
                result, calls, _ = self.run_helper(script, "cap_chown,cap_net_bind_service=ep", capbnd="00000000a80421fb")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, "")
                self.assertIn("other capabilities", result.stderr)

    def test_all_provider_swap_sites_reapply(self):
        installer = (ROOT / SCRIPTS[0]).read_text()
        self.assertEqual(installer.count('ensure_extender_cap "$install_path/bin/urnetwork"'), 4)
        self.assertIn('ensure_extender_cap "$provider_bin"', (ROOT / SCRIPTS[2]).read_text())
        self.assertIn('ensure_extender_cap "$APP_DIR/urnetwork_${A_SYS_ARCH}_${PROVIDER_FLAVOR}"',
                      (ROOT / SCRIPTS[1]).read_text())


if __name__ == "__main__":
    unittest.main(verbosity=2)
