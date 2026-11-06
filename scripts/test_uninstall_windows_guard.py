#!/usr/bin/env python3
"""Exercise the uninstaller's real process guard with mocked CIM calls."""
import base64
from pathlib import Path
import shutil
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
POWERSHELL = shutil.which("pwsh") or shutil.which("powershell.exe")


@unittest.skipUnless(POWERSHELL, "PowerShell is unavailable")
class UninstallProcessGuardTest(unittest.TestCase):
    def run_guard(self, scenario):
        source = (ROOT / "scripts/Provider_Uninstall_Win32.ps1").read_text()
        guard = source[source.index('$ProviderExe = Join-Path'):source.index('# Kill the legacy updater')]
        prefix = r'''
$InstallDir = 'C:\Fake'
$script:queries = 0
function Get-CimInstance {
    param($ClassName, $ErrorAction)
    $script:queries++
    if ($env:GUARD_SCENARIO -eq 'query-error' -or ($env:GUARD_SCENARIO -eq 'query-error-after' -and $script:queries -gt 1)) { throw 'query failed' }
    if (($env:GUARD_SCENARIO -eq 'disappears' -or $env:GUARD_SCENARIO -eq 'terminate-error' -or $env:GUARD_SCENARIO -eq 'query-error-after') -and $script:queries -eq 1) {
        return [pscustomobject]@{ ExecutablePath = 'C:\Fake\urnetwork.exe'; ProcessId = 42 }
    }
}
function Invoke-CimMethod { param($InputObject, $MethodName, $ErrorAction) if ($env:GUARD_SCENARIO -eq 'terminate-error') { throw 'terminate failed' }; return [pscustomobject]@{ ReturnValue = 0 } }
'''
        program = "$env:GUARD_SCENARIO = '" + scenario + "'\n" + prefix + guard + "\nWrite-Output 'CLEANUP_REACHED'\n"
        encoded = base64.b64encode(program.encode("utf-16le")).decode()
        import os
        env = dict(os.environ, GUARD_SCENARIO=scenario)
        return subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded],
                              env=env, capture_output=True, text=True)

    def test_query_failure_refuses_cleanup(self):
        for scenario in ("query-error", "query-error-after", "terminate-error"):
            with self.subTest(scenario=scenario):
                result = self.run_guard(scenario)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("CLEANUP_REACHED", result.stdout)

    def test_process_disappears_before_cleanup(self):
        result = self.run_guard("disappears")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("CLEANUP_REACHED", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
