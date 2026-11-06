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
        source = (ROOT / "scripts/Provider_Uninstall_Win32.ps1").read_text(encoding="utf-8")
        guard = source[source.index('$ProviderExe = Join-Path'):source.index('# Kill the legacy updater')]
        prefix = r'''
$InstallDir = 'C:\Fake'
$Arch = 'amd64'
$script:queries = 0
$script:clock = 0
function Get-Date {
    if ($env:GUARD_SCENARIO -eq 'still-alive') {
        $script:clock++
        return ([datetime]'2026-01-01').AddSeconds($script:clock * 20)
    }
    return Microsoft.PowerShell.Utility\Get-Date
}
function Start-Sleep { param($Milliseconds) }
function Get-CimInstance {
    [CmdletBinding()] param($ClassName)
    $script:queries++
    if ($env:GUARD_SCENARIO -eq 'query-error' -or ($env:GUARD_SCENARIO -eq 'query-error-after' -and $script:queries -gt 1)) { Write-Error 'query failed'; return }
    if ($env:GUARD_SCENARIO -eq 'legacy' -and $script:queries -eq 1) { return [pscustomobject]@{ Name = 'urnetwork.exe'; ExecutablePath = (Join-Path $InstallDir 'windows\amd64\urnetwork.exe'); ProcessId = 42 } }
    if ($env:GUARD_SCENARIO -eq 'unreadable') { return [pscustomobject]@{ Name = 'urnetwork.exe'; ExecutablePath = $null; ProcessId = 99 } }
    if ($env:GUARD_SCENARIO -eq 'other-provider') { return [pscustomobject]@{ Name = 'urnetwork.exe'; ExecutablePath = 'C:\Other\urnetwork.exe'; ProcessId = 99 } }
    if (($env:GUARD_SCENARIO -eq 'terminate-error' -or $env:GUARD_SCENARIO -eq 'terminate-nonzero' -or $env:GUARD_SCENARIO -eq 'still-alive') -or (($env:GUARD_SCENARIO -eq 'disappears' -or $env:GUARD_SCENARIO -eq 'vanished-before-terminate' -or $env:GUARD_SCENARIO -eq 'query-error-after') -and $script:queries -eq 1)) {
        return [pscustomobject]@{ Name = 'urnetwork.exe'; ExecutablePath = $ProviderExe; ProcessId = 42 }
    }
}
function Invoke-CimMethod { [CmdletBinding()] param($InputObject, $MethodName) Write-Host "TERMINATED:$($InputObject.ProcessId)"; if ($env:GUARD_SCENARIO -eq 'terminate-error' -or $env:GUARD_SCENARIO -eq 'vanished-before-terminate') { Write-Error 'terminate failed'; return }; if ($env:GUARD_SCENARIO -eq 'terminate-nonzero') { return [pscustomobject]@{ ReturnValue = 2 } }; return [pscustomobject]@{ ReturnValue = 0 } }
'''
        program = "$env:GUARD_SCENARIO = '" + scenario + "'\n" + prefix + guard + "\nWrite-Output 'CLEANUP_REACHED'\n"
        encoded = base64.b64encode(program.encode("utf-16le")).decode()
        import os
        env = dict(os.environ, GUARD_SCENARIO=scenario)
        return subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded],
                              env=env, capture_output=True, text=True)

    def test_architecture_query_failure_is_reported(self):
        source = (ROOT / "scripts/Provider_Uninstall_Win32.ps1").read_text(encoding="utf-8")
        segment = source[source.index('$Arch = (Get-CimInstance'):source.index('Write-Debug "Detected architecture:')]
        program = "function Get-CimInstance { [CmdletBinding()] param($ClassName) Write-Error 'processor query failed' }\n" + segment + "\nWrite-Output 'ARCH_SUCCESS'\n"
        encoded = base64.b64encode(program.encode("utf-16le")).decode()
        result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("ARCH_SUCCESS", result.stdout)
        self.assertIn("processor query failed", result.stderr)

    def test_query_failure_refuses_cleanup(self):
        for scenario in ("query-error", "query-error-after", "terminate-error", "terminate-nonzero", "unreadable", "other-provider", "still-alive"):
            with self.subTest(scenario=scenario):
                result = self.run_guard(scenario)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("CLEANUP_REACHED", result.stdout)

    def test_process_vanishes_before_terminate(self):
        result = self.run_guard("vanished-before-terminate")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("TERMINATED:42", result.stdout)
        self.assertIn("CLEANUP_REACHED", result.stdout)

    def test_legacy_installed_provider_is_stopped(self):
        result = self.run_guard("legacy")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("TERMINATED:42", result.stdout)
        self.assertIn("CLEANUP_REACHED", result.stdout)

    def test_process_disappears_before_cleanup(self):
        result = self.run_guard("disappears")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("TERMINATED:42", result.stdout)
        self.assertIn("CLEANUP_REACHED", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
