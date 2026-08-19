#!/usr/bin/env pwsh
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# urnet-tools -- URnetwork provider manager (also acts as an installation script)
# GitHub: <https://github.com/full-bars/urnetwork-3.23-fix>

param(
    [String]$Version = "latest",
    [String]$Destination = "",
    [String]$ToolsScriptPath = "",
    [String]$UpdaterScriptPath = "",
    [Switch]$NoRestartDownload = $false,
    [Switch]$NoCleanup = $false,
    [Switch]$NonInteractive = $false,
    [Switch]$AddToStartup = $false
);

$Bold = ""
$Reset = ""

if ($PSStyle) {
    $Bold = $PSStyle.Bold
    $Reset = $PSStyle.Reset
}

if ($Version -contains "/" -or $Version -contains "\") {
    Write-Error "Version must not contain a forward-slash or backslash"
    exit 1
}

$OS = ""

if ($IsLinux) {
    $OS = "linux"
}
else {
    $OS = "windows"
}

if ($OS -ne "windows") {
    Write-Host "Note: This script is supposed to be used on Windows systems, support for other platforms only exist for the ease of development of the script itself."
}

if (-not $Destination) {
    if ($OS -eq "linux") {
	$Destination = Join-Path -Path $env:HOME -ChildPath ".local/share/urnetwork"
    }
    else {
	$Destination = Join-Path -Path $env:LOCALAPPDATA -ChildPath "urnetwork\provider"
    }
}

if ($OS -eq "linux") {
    $env:TEMP = "/tmp"
}

$Arch = switch ($OS) {
    "windows" {
	switch ((Get-CimInstance Win32_Processor).Architecture) {
	    9 { "amd64" }
	    12 { "arm64" }
	    default { "unsupported" }
	}
    }
    default {
	switch ((uname -m)) {
	    "x86_64" { "amd64" }
	    "aarch64" { "arm64" }
	    default { "unsupported" }
	}
    }
}

if ($Arch -eq "unsupported") {
    Write-Error "Unsupported architecture: $Arch"
    exit 1
}

function Print-Settings {
    Write-Host "Installation options:"
    Write-Host ""
    Write-Host "Version:      $Version"
    Write-Host "Destination:  $Destination"
    Write-Host "OS:           $OS"
    Write-Host "Architecture: $Arch"
    Write-Host ""
}

function Download-File {
    param(
	[String]$URL,
	[String]$Destination
    );

    Write-Host "Downloading $URL => $Destination"

    if ($OS -ne "linux") {
	try {
	    Start-BitsTransfer -Source $URL -Destination $Destination

	    if ($?) {
		return
	    }
	}
	catch {}

	Write-Host "Download via BITS failed. Falling back to using a normal web request"
    }
    
    Invoke-WebRequest -Uri $URL -OutFile $Destination
}

function Get-Path {
    return [Environment]::GetEnvironmentVariable("PATH", [System.EnvironmentVariableTarget]::User)
}

function Set-Path {
    param (
        [Parameter(Mandatory = $true)]
        [String]$Value
    )

    [Environment]::SetEnvironmentVariable("PATH", $Value, [System.EnvironmentVariableTarget]::User)
}

Print-Settings

$GithubURLBase = "https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

if ($Version -eq "latest") {
    $GithubURL = "$GithubURLBase/releases/latest"
}
else {
    $GithubURL = "$GithubURLBase/releases/tags/$Version"
}

$ReleaseInfo = $null
try {
    $ReleaseInfo = Invoke-RestMethod -Uri "$GithubURL"
}
catch {}

$ReleaseVersion = $null
$ReleaseDate = $null
$DownloadURL = $null
$FileName = $null

if ($ReleaseInfo) {
    $ReleaseVersion = $ReleaseInfo.tag_name
    $ReleaseDate = $ReleaseInfo.published_at
    $OSArchAssetName = "urnetwork-provider-$ReleaseVersion-$OS-$Arch.tar.gz"
    $ReleaseAsset = $ReleaseInfo.assets | Where-Object { $_.name -eq $OSArchAssetName }
    if (-not $ReleaseAsset) {
        $ReleaseAsset = $ReleaseInfo.assets | Where-Object { $_.name -eq "urnetwork-provider-$ReleaseVersion.tar.gz" }
    }
    if (-not $ReleaseAsset) {
        $ReleaseAsset = $ReleaseInfo.assets | Where-Object { $_.name -match "^urnetwork-provider-.*\.tar\.gz$" } | Select-Object -First 1
    }
    if ($ReleaseAsset) {
        $MirrorURL = $ReleaseAsset.browser_download_url
        $FileName = $ReleaseAsset.name
        $DownloadURL = "https://dl.fullbars.xyz/releases/download/$ReleaseVersion/$FileName"
    }
}

# GitHub API failed, was rate-limited, or the release has no matching asset:
# for an explicit version we already know the tag; for "latest" fall back to
# the dl.fullbars.xyz Worker, which mirrors GitHub's latest-release tag at
# the edge. Either way, construct the download URL directly — the release
# tarball layout is fixed (urnetwork-provider-<tag>.tar.gz).
if (-not $DownloadURL) {
    if ($Version -ne "latest") {
        $ReleaseVersion = $Version
    }
    else {
        Write-Warning "GitHub API unavailable, trying dl.fullbars.xyz fallback..."
        try {
            $ReleaseVersion = (Invoke-RestMethod -Uri "https://dl.fullbars.xyz/latest-version").Trim()
        }
        catch {}
    }

    if (-not $ReleaseVersion) {
        Write-Error "Failed to fetch release information from GitHub API. Are you sure the version exists and your internet connection is working?"
        exit 1
    }

    $FileName = "urnetwork-provider-$ReleaseVersion.tar.gz"
    $DownloadURL = "https://dl.fullbars.xyz/releases/download/$ReleaseVersion/$FileName"
    $MirrorURL = "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$ReleaseVersion/$FileName"
}

$FilePath = Join-Path -Path $env:TEMP -ChildPath ([string]$FileName)

if (-not $NoRestartDownload -or -not (Test-Path $FilePath)) {
    try {
        Download-File -URL $DownloadURL -Destination $FilePath
    }
    catch {
        Write-Warning "Primary download failed, trying GitHub mirror..."
        Download-File -URL $MirrorURL -Destination $FilePath
    }
}

$ExtractPath = Join-Path $env:TEMP -ChildPath "urnetwork-extracted"

if (Test-Path $ExtractPath) {
    Remove-Item -Path $ExtractPath -Recurse -Force
}

$null = New-Item -Path $ExtractPath -ItemType Directory

Write-Host "Extracting $FilePath => $ExtractPath"
tar -xzf $FilePath -C $ExtractPath

$BinarySuffix = switch ($OS) {
    "linux" { "" }
    "windows" { ".exe" }
}

$BinaryPath = Join-Path $ExtractPath -ChildPath "$OS/$Arch/provider$BinarySuffix"

if (-not (Test-Path $BinaryPath)) {
    Write-Error "File $BinaryPath not found: The downloaded archive file is most likely corrupt"
    exit 1
}

$InstalledBinaryPath = Join-Path $Destination -ChildPath "urnetwork$BinarySuffix"
$VersionFile = Join-Path $Destination -ChildPath "version"
$InstallDateFile = Join-Path $Destination -ChildPath "date"

if (-not (Test-Path $Destination)) {
    New-Item -Path $Destination -ItemType Directory
}

if (Test-Path $InstalledBinaryPath) {
    Remove-Item -Path $InstalledBinaryPath -Force
}

Write-Host "Installing $BinaryPath => $InstalledBinaryPath"
Move-Item -Path $BinaryPath $InstalledBinaryPath

$InstalledToolsPath = Join-Path $Destination -ChildPath "urnet-tools.ps1"
$InstalledToolsBinaryPath = Join-Path $Destination -ChildPath "urnet-tools.exe"

# The tool is now a Go binary shipped as a standalone release asset
# (urnet-tools-windows-<arch>, v3.23.0-fix.28+). Prefer it — digest-verified
# against the release API — and fall back to the legacy PS1 scripts only for
# releases that predate the Go asset. The Go tool is self-updating
# (`urnet-tools update` refreshes its own binary), so this PS1 path is a
# one-time handoff, mirroring Provider_Install_Linux.sh.
# NOTE: the fork does NOT publish a separate urnetwork-updater binary;
# auto-update is a systemd timer on Linux. On Windows, auto-update-enable is
# a no-op/error until a Windows scheduling mechanism lands in the Go tool.
$ToolGoInstalled = $false
$ToolAssetName = "urnet-tools-windows-$Arch"
$ToolAsset = $null
$ToolDigest = ""
if ($ReleaseInfo) {
    $ToolAsset = $ReleaseInfo.assets | Where-Object { $_.name -eq $ToolAssetName }
    if ($ToolAsset) {
        $ToolDigest = $ToolAsset.digest
    }
}

if ($ToolAsset -and $ToolDigest) {
    # Go tool path: download, verify, swap with .old (Windows cannot delete a
    # running mapped image, but can rename it).
    $ToolDownloadURL = "https://dl.fullbars.xyz/releases/download/$ReleaseVersion/$ToolAssetName"
    $ToolTemp = Join-Path $env:TEMP $ToolAssetName
    Write-Host "Installing Go urnet-tools binary ($ToolAssetName)..."
    try {
        Download-File -URL $ToolDownloadURL -Destination $ToolTemp
        $ActualHash = (Get-FileHash -Path $ToolTemp -Algorithm SHA256).Hash.ToLower()
        # The release API digest is "sha256:<hex>"; strip the prefix.
        $ExpectedHash = $ToolDigest.ToLower()
        if ($ExpectedHash -like "sha256:*") {
            $ExpectedHash = $ExpectedHash.Substring(7)
        }
        if ($ActualHash -eq $ExpectedHash) {
            if (Test-Path $InstalledToolsBinaryPath) {
                Move-Item -Path $InstalledToolsBinaryPath -Destination "$InstalledToolsBinaryPath.old" -Force
            }
            Move-Item -Path $ToolTemp -Destination $InstalledToolsBinaryPath
            # Remove the legacy PS1 pair so the Go tool is the only manager.
            if (Test-Path $InstalledToolsPath) {
                Remove-Item -Path $InstalledToolsPath -Force
            }
            $StaleUpdaterPs1 = Join-Path $Destination -ChildPath "urnetwork-updater.ps1"
            if (Test-Path $StaleUpdaterPs1) {
                Remove-Item -Path $StaleUpdaterPs1 -Force
            }
            # Point auto-update-enable at the Go binary.
            $InstalledToolsPath = $InstalledToolsBinaryPath
            $ToolGoInstalled = $true
        }
        else {
            Write-Warning "Go urnet-tools digest mismatch (got $ActualHash); falling back to PS1"
            Remove-Item -Path $ToolTemp -Force -ErrorAction SilentlyContinue
        }
    }
    catch {
        Write-Warning "Failed to install Go urnet-tools; falling back to PS1: $($_.Exception.Message)"
        Remove-Item -Path $ToolTemp -Force -ErrorAction SilentlyContinue
    }
}

if (-not $ToolGoInstalled) {
    if (Test-Path $InstalledToolsPath) {
        Remove-Item -Path $InstalledToolsPath -Force
    }

    Write-Host "Installing urnet-tools => $InstalledToolsPath"

    if ($ToolsScriptPath) {
        Copy-Item $ToolsScriptPath $InstalledToolsPath
    }
    else {
        Invoke-RestMethod "https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/urnet-tools.ps1" -OutFile $InstalledToolsPath
    }

    $InstalledUpdaterPath = Join-Path $Destination -ChildPath "urnetwork-updater.ps1"

    if (Test-Path $InstalledUpdaterPath) {
        Remove-Item -Path $InstalledUpdaterPath -Force
    }

    Write-Host "Installing urnetwork-updater => $InstalledUpdaterPath"

    if ($UpdaterScriptPath) {
        Copy-Item $UpdaterScriptPath $InstalledUpdaterPath
    }
    else {
        Invoke-RestMethod "https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/urnetwork-updater.ps1" -OutFile $InstalledUpdaterPath
    }
}

Set-Content $VersionFile $ReleaseVersion
Set-Content $InstallDateFile $ReleaseDate

if ($Version -eq "latest") {
    Write-Host "Running: urnet-tools auto-update weekly"
    & $InstalledToolsPath auto-update weekly
    if (-not $?) {
        Write-Warning "auto-update enable failed (exit $LASTEXITCODE); continuing install"
    }
}
else {
    Write-Host "Not enabling auto update since a version other than 'latest' was installed."
}

if ($OS -eq "windows") {
    $CurrentPath = Get-Path
    $CurrentPathSplitted = $CurrentPath.Split(";")

    if (-not ($CurrentPathSplitted -contains $Destination)) {
	Write-Host "Adding $Destination to %PATH%"
    
	$Colon = ";";

	if ($EnvPath -match ";$") {
            $Colon = "";
	}

	$NewPath = "$CurrentPath$Colon$Destination;"
	Set-Path -Value $NewPath

	if (-not $?) {
            Write-Error "Failed to update %PATH%"
            exit 1
	}
    }
}
else {
    Write-Host "Not updating `$PATH variable automatically -- leaving that on you"
    Write-Host "Add the following path to your `$PATH: $Destination"
}

if (-not $NoCleanup) {
    Write-Host "Cleaning up temporary files"
    Remove-Item -Path $FilePath
    Remove-Item -Path $ExtractPath -Recurse -Force
}

Write-Host "Installation complete! Restart your terminal or command-line for the changes to take effect."

Write-Host "$($Bold)Start in foreground:$($Reset) urnetwork provide"
Write-Host "$($Bold)Start in background:$($Reset) urnet-tools start"
Write-Host "$($Bold)Authenticate:$($Reset)        urnetwork auth"
Write-Host "$($Bold)More help:$($Reset)           urnetwork --help"

if (-not $NonInteractive) {
    $DataDir = ""

    if ($OS -eq "windows") {
	$DataDir = "$env:HOMEDRIVE$env:HOMEPATH\.urnetwork"
    }
    else {
	$DataDir = "$env:HOME/.urnetwork"
    }
    
    if (Test-Path $DataDir) {
	Write-Host "Found data directory at $DataDir"
	Write-Host "Skipping authentication"
    }
    else {
	$Answer = Read-Host "Would you like to authenticate to URnetwork now? [Y/n]"

	if ($Answer.ToLower() -eq "y") {
	    Write-Host "Authenticating now"

	    while ($true) {
		& $InstalledBinaryPath auth

		if ($?) {
		    break
		}

		Write-Host "Trying again"
	    }
	}
    }
}

if ($OS -eq "windows") {
    $Answer = "n"
    
    if ($AddToStartup) {
	$Answer = "y"
    }
    elseif (-not $NonInteractive) {
	$Answer = Read-Host "Do you want to add this service to startup? [Y/n]"
    }
    
    if ($Answer.ToLower() -eq "y") {
	# Prefer the Go tool's schtasks onlogon task; fall back to the legacy
	# Startup-folder .lnk only when the Go tool is not installed (old
	# PS1-managed installs).
	if ($ToolGoInstalled -and (Test-Path $InstalledToolsBinaryPath)) {
	    Write-Host "Enabling auto-start via Task Scheduler (urnet-tools auto-start on)"
	    & $InstalledToolsBinaryPath auto-start on
	    if (-not $?) {
		Write-Warning "auto-start enable failed (exit $LASTEXITCODE)"
	    }
	}
	else {
	    $StartupPath = Join-Path -Path $env:APPDATA -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"
	    $ShortcutPath = Join-Path -Path $StartupPath -ChildPath "urnetwork.lnk"

	    if (Test-Path $ShortcutPath) {
		Remove-Item -Path $ShortcutPath -Force
	    }

	    $StartCommand = "Start-Process -FilePath '$InstalledBinaryPath' -ArgumentList 'provide' -WindowStyle Hidden"
	    $Arguments = '-NoProfile -WindowStyle Hidden -Command "' + $StartCommand + '"'

	    Write-Host "Startup command: powershell.exe $Arguments"

	    $WshShell = New-Object -ComObject WScript.Shell
	    $Shortcut = $WshShell.CreateShortcut($ShortcutPath)
	    $Shortcut.TargetPath = "powershell.exe"
	    $Shortcut.Arguments = $Arguments
	    $Shortcut.WorkingDirectory = $Destination
	    $Shortcut.WindowStyle = 7
	    $Shortcut.Save()

	    Write-Host "Added URnetwork provider to startup"
	}
    }
}
