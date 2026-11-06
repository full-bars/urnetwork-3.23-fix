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

$Arch = (Get-CimInstance Win32_Processor -ErrorAction Stop).Architecture
# Windows can report more than one physical processor. Use one architecture
# only when every reported processor agrees; otherwise refuse removal.
$Architectures = @($Arch | Where-Object { $null -ne $_ } | Select-Object -Unique)
if ($Architectures.Count -ne 1) {
    Write-Error "Could not determine a single processor architecture. Nothing was removed."
    exit 1
}
$Arch = switch ($Architectures[0]) {
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
$ProviderExes = @($ProviderExe, (Join-Path -Path $InstallDir -ChildPath "windows\$Arch\urnetwork.exe"))
# Stop the installed provider and prove it has exited before removing files.
# A failed process query is not evidence that no provider is running.
try {
    $AllProviders = @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object { $_.Name -eq 'urnetwork.exe' })
    $OtherProviders = @($AllProviders | Where-Object { -not $_.ExecutablePath -or $ProviderExes -notcontains $_.ExecutablePath })
    if ($OtherProviders.Count -gt 0) { throw "Another provider is running or its path cannot be verified; stop it before uninstalling" }
    $Processes = @($AllProviders | Where-Object { $ProviderExes -contains $_.ExecutablePath })
    foreach ($Process in $Processes) {
        try {
            $Result = Invoke-CimMethod -InputObject $Process -MethodName Terminate -ErrorAction Stop
            if ($Result.ReturnValue -ne 0) { throw "Terminate returned $($Result.ReturnValue) for PID $($Process.ProcessId)" }
        } catch {
            # A process may exit between discovery and termination. Only
            # suppress the failure after a successful query proves the PID gone.
            $SamePid = @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object { $_.ProcessId -eq $Process.ProcessId })
            if ($SamePid.Count -gt 0) { throw }
        }
    }
    $Deadline = (Get-Date).AddSeconds(15)
    do {
        $Alive = @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object { $_.Name -eq 'urnetwork.exe' })
        if ($Alive.Count -eq 0) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $Deadline)
    if ($Alive.Count -gt 0) { throw "The provider is still running" }
} catch {
    Write-Error "Could not verify the provider has stopped. Nothing was removed. $($_.Exception.Message)"
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
