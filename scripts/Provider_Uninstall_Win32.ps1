#!/usr/bin/env pwsh

param(
    [Switch]$ForAllUsers = $false
);

$CurrentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())

if ($ForAllUsers) {
    if (-not $CurrentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Host "Not running as admin. Please run with elevated admin permissions."
        exit 1
    }
    else {
        Write-Host "Running as admin"
    }
}

$Arch = (Get-CimInstance Win32_Processor).Architecture
$Arch = switch ($Arch) {
    9 { "amd64" }
    12 { "arm64" }
    default { "unsupported" }
}

Write-Debug "Detected architecture: $Arch"

if ($Arch -eq "unsupported") {
    Write-Error "Unsupported architecture. Urnetwork only supports Windows on x64 and ARM64."
    exit 1
}

$DataDir = "$env:HOMEDRIVE$env:HOMEPATH\.urnetwork"
$InstallDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"

if ($ForAllUsers) {
    $InstallDir = "C:\Program Files\urnetwork\provider"
}

if (-not (Test-Path $InstallDir)) {
    Write-Error "Urnetwork installation could not be detected on this PC."
    exit 1
}

$ProviderExe = Join-Path -Path $InstallDir -ChildPath "urnetwork.exe"
# Terminate the running provider. The installer always places urnetwork.exe
# flat in the install dir (the release tarball is flat), so match the exact
# installed path only.
Get-WmiObject Win32_Process | Where-Object { $_.ExecutablePath -eq $ProviderExe } | ForEach-Object { $_.Terminate() }

# Terminate() returns before the process is gone. Wait for it, so the files
# below are never removed under a live provider, and stop here if it stays.
$Deadline = (Get-Date).AddSeconds(15)
do {
    $Alive = @(Get-WmiObject Win32_Process | Where-Object { $_.ExecutablePath -eq $ProviderExe })
    if ($Alive.Count -eq 0) { break }
    Start-Sleep -Milliseconds 250
} while ((Get-Date) -lt $Deadline)
if ($Alive.Count -gt 0) {
    $Pids = ($Alive | ForEach-Object { $_.ProcessId }) -join ", "
    Write-Error "The provider ($ProviderExe, PID $Pids) is still running. Nothing was removed. End it in Task Manager, then run this uninstaller again."
    exit 1
}

# Kill the legacy updater process (PS1-era) if still running.
# Read the PID file first — the process runs under powershell.exe,
# not "urnetwork-updater", so Get-Process never matches.
$StalePid = Join-Path -Path $InstallDir -ChildPath "urnetwork-updater.pid"
if (Test-Path $StalePid) {
    try {
        $OldPid = ([string](Get-Content -Path $StalePid -Raw)).Trim()
        if ($OldPid -match '^\d+$' -and [int]$OldPid -ne $PID) {
            $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $OldPid" -ErrorAction SilentlyContinue
            if ($proc -and $proc.CommandLine -match 'urnetwork-updater') {
                Write-Host "Terminating stale updater process (PID $OldPid, Name: $($proc.Name))"
                Stop-Process -Id ([int]$OldPid) -Force -ErrorAction SilentlyContinue
            }
        }
    } catch {}
    Remove-Item -Path $StalePid -Force -ErrorAction SilentlyContinue
}

# Remove Task Scheduler tasks created by the Go urnet-tools binary.
# The Go tool's cleanupLifecycle does this too, but the uninstaller
# must be thorough even on partially-managed installs.
schtasks /Delete /TN "urnetwork-update" /F *>$null
schtasks /Delete /TN "urnetwork-autostart" /F *>$null

Write-Host "Removing installation directory: $InstallDir"
Remove-Item -Path $InstallDir -Recurse -Force

if (!$?) {
    Write-Error "Could not remove the installation directory"
    exit 1
}

if (Test-Path $DataDir) {
    Write-Host "Removing data directory: $DataDir"
    Remove-Item -Path $DataDir -Recurse -Force

    if (!$?) {
        Write-Error "Could not remove the data directory"
        exit 1
    }
}

$StartupPath = Join-Path -Path $env:APPDATA -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"

if ($ForAllUsers) {
    $StartupPath = Join-Path -Path "C:\ProgramData" -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"
}

$ShortcutPath = Join-Path -Path $StartupPath -ChildPath "urnetwork.lnk"

if (Test-Path $ShortcutPath) {
    Write-Host "Removing startup entry: $ShortcutPath"
    Remove-Item -Path $ShortcutPath -Force

    if (!$?) {
        Write-Error "Could not remove the startup entry"
        exit 1
    }
}

# Also remove the legacy update shortcut (created by urnet-tools.ps1).
$UpdateShortcut = Join-Path -Path $StartupPath -ChildPath "urnetwork-update.lnk"
if (Test-Path $UpdateShortcut) {
    Write-Host "Removing startup entry: $UpdateShortcut"
    Remove-Item -Path $UpdateShortcut -Force -ErrorAction SilentlyContinue
}

function Get-Path {
    if ($ForAllUsers) {
        return [System.Environment]::GetEnvironmentVariable("PATH", [System.EnvironmentVariableTarget]::Machine)
    }

    return [Environment]::GetEnvironmentVariable("PATH", [System.EnvironmentVariableTarget]::User)
}

function Set-Path {
    param (
        [Parameter(Mandatory = $true)]
        [String]$Value
    )

    if ($ForAllUsers) {
        [System.Environment]::SetEnvironmentVariable("PATH", $Value, [System.EnvironmentVariableTarget]::Machine)
    }
    else {
        [Environment]::SetEnvironmentVariable("PATH", $Value, [System.EnvironmentVariableTarget]::User)
    }
}

# The installer adds $InstallDir to PATH. Some legacy installs used
# $InstallDir\windows\$Arch instead — remove both.
$OldPaths = @($InstallDir, "$InstallDir\windows\$Arch")
$EnvPath = Get-Path
$EnvPath = if ($EnvPath) { $EnvPath } else { '' }
$EnvPathSplitted = $EnvPath.Split(";")

$Removed = $false
foreach ($OldPath in $OldPaths) {
    if ($EnvPathSplitted -contains $OldPath) {
        $EnvPathSplitted = @($EnvPathSplitted | Where-Object { $_ -ne $OldPath })
        $Removed = $true
    }
}

if ($Removed) {
    Write-Host "Updating PATH variable"
    $NewPath = ($EnvPathSplitted | Where-Object { $_ -ne "" }) -join ';'
    Set-Path -Value $NewPath

    if (!$?) {
        Write-Error "Failed to update PATH variable. Please check your permissions."
        exit 1
    }
}

Write-Host "Uninstallation successful."
Write-Host "If Urnetwork provider is already running, you can terminate it from Task Manager. Otherwise, you can just restart your PC to stop it."
