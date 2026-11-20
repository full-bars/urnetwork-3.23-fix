#!/usr/bin/env pwsh
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# urnet-tools -- URnetwork provider manager (also acts as an installation script)
# GitHub: <https://github.com/full-bars/urnetwork-3.23-fix>

param(
    [String]$Version = "latest",
    [String]$Destination = "",

    [Switch]$NoRestartDownload = $false,
    [Switch]$NoCleanup = $false,
    [Switch]$NoUTAlias = $false,
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
        $ReleaseAsset = $ReleaseInfo.assets | Where-Object { $_.name -match "^urnetwork-provider-.*\-${OS}-${Arch}\.tar\.gz$" } | Select-Object -First 1
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

$BinaryPath = Join-Path $ExtractPath -ChildPath "provider$BinarySuffix"
if (-not (Test-Path $BinaryPath)) {
    # Some release tarballs nest the binary under <os>/<arch>/.
    $NestedBinaryPath = Join-Path $ExtractPath -ChildPath "$OS/$Arch/provider$BinarySuffix"
    if (Test-Path $NestedBinaryPath) {
        $BinaryPath = $NestedBinaryPath
    }
}

if (-not (Test-Path $BinaryPath)) {
    Write-Error "File $BinaryPath not found: The downloaded archive file is most likely corrupt"
    exit 1
}

# The release tarballs name the binary provider<.exe> (both the flat per-arch
# tarball and the nested universal one), but the fork installs it under the
# stable documented name urnetwork<.exe> -- every consumer (the install
# quickstart, urnet-tools process discovery, the uninstaller, docs/README)
# assumes that name. Rename on install so the on-disk contract never changes.
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

$InstalledToolsBinaryPath = Join-Path $Destination -ChildPath "urnet-tools.exe"
$UTAliasPath = Join-Path $Destination -ChildPath "ut.exe"
$UTAliasItem = Get-Item -LiteralPath $UTAliasPath -Force -ErrorAction SilentlyContinue
$UTAliasOwned = $null -eq $UTAliasItem
if ($null -ne $UTAliasItem -and -not ($UTAliasItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -and (Test-Path -LiteralPath $UTAliasPath -PathType Leaf) -and (Test-Path -LiteralPath $InstalledToolsBinaryPath -PathType Leaf)) {
    $UTAliasOwned = (Get-FileHash -LiteralPath $UTAliasPath).Hash -eq (Get-FileHash -LiteralPath $InstalledToolsBinaryPath).Hash
}

# The tool is a Go binary shipped as a standalone release asset
# (urnet-tools-windows-<arch>, v3.23.0-fix.28+). Digest-verified
# against the release API. The Go tool is self-updating
# (`urnet-tools update` refreshes its own binary).
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

# Helper: tear down legacy PS1 updater and clean up Startup .lnk.
# Called after urnet-tools.exe is installed from any source.
$CleanupStaleUpdater = {
    # Remove any stale PS1 scripts from previous installs.
    $StalePs1 = Join-Path $Destination -ChildPath "urnet-tools.ps1"
    if (Test-Path $StalePs1) { Remove-Item -Path $StalePs1 -Force }
    $StaleUpdaterPs1 = Join-Path $Destination -ChildPath "urnetwork-updater.ps1"
    if (Test-Path $StaleUpdaterPs1) { Remove-Item -Path $StaleUpdaterPs1 -Force }

    # Read PID file and terminate legacy background updater.
    $StalePid = Join-Path $Destination -ChildPath "urnetwork-updater.pid"
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

    # Remove the legacy Startup .lnk files — the Go tool uses Task
    # Scheduler (schtasks /on logon) instead.
    $StartupPath = Join-Path -Path $env:APPDATA -ChildPath "Microsoft\Windows\Start Menu\Programs\Startup"
    $OldLnk = Join-Path -Path $StartupPath -ChildPath "urnetwork.lnk"
    if (Test-Path $OldLnk) { Remove-Item -Path $OldLnk -Force -ErrorAction SilentlyContinue }
    $UpdateLnk = Join-Path -Path $StartupPath -ChildPath "urnetwork-update.lnk"
    if (Test-Path $UpdateLnk) { Remove-Item -Path $UpdateLnk -Force -ErrorAction SilentlyContinue }
}

# STEP 1: Check if urnet-tools.exe was already bundled inside the
# extracted provider tarball (v3.23.0-fix.30+). This is the
# preferred path — no extra download required.
$ExtractedTool = Join-Path $ExtractPath -ChildPath "urnet-tools.exe"
if (-not (Test-Path $ExtractedTool)) {
    # Some tarballs nest binaries under <os>/<arch>/.
    $NestedTool = Join-Path $ExtractPath -ChildPath "$OS/$Arch/urnet-tools.exe"
    if (Test-Path $NestedTool) {
        $ExtractedTool = $NestedTool
    }
}

if (Test-Path $ExtractedTool) {
    Write-Host "Found urnet-tools.exe in extracted tarball ($ExtractedTool)"
    if (Test-Path $InstalledToolsBinaryPath) {
        $OldBinary = "$InstalledToolsBinaryPath.old"
        if (Test-Path $OldBinary) {
            Remove-Item -Path $OldBinary -Force -ErrorAction SilentlyContinue
        }
        Move-Item -Path $InstalledToolsBinaryPath -Destination $OldBinary -Force
    }
    Move-Item -Path $ExtractedTool -Destination $InstalledToolsBinaryPath -Force
    $ToolGoInstalled = $true
    & $CleanupStaleUpdater
}

# STEP 2: Fallback — download standalone urnet-tools asset from
# dl.fullbars.xyz with GitHub releases mirror.
if (-not $ToolGoInstalled -and $ToolAsset -and $ToolDigest) {
    $ToolDownloadURL = "https://dl.fullbars.xyz/releases/download/$ReleaseVersion/$ToolAssetName"
    $ToolMirrorURL = "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$ReleaseVersion/$ToolAssetName"
    $ToolTemp = Join-Path $env:TEMP $ToolAssetName
    Write-Host "Installing Go urnet-tools binary ($ToolAssetName) via download..."
    try {
        try {
            Download-File -URL $ToolDownloadURL -Destination $ToolTemp
        } catch {
            Write-Warning "Primary tool download failed, trying GitHub mirror..."
            Download-File -URL $ToolMirrorURL -Destination $ToolTemp
        }
        $ActualHash = (Get-FileHash -Path $ToolTemp -Algorithm SHA256).Hash.ToLower()
        $ExpectedHash = $ToolDigest.ToLower()
        if ($ExpectedHash -like "sha256:*") {
            $ExpectedHash = $ExpectedHash.Substring(7)
        }
        if ($ActualHash -eq $ExpectedHash) {
            if (Test-Path $InstalledToolsBinaryPath) {
                $OldBinary = "$InstalledToolsBinaryPath.old"
                if (Test-Path $OldBinary) {
                    Remove-Item -Path $OldBinary -Force -ErrorAction SilentlyContinue
                }
                Move-Item -Path $InstalledToolsBinaryPath -Destination $OldBinary -Force
            }
            Move-Item -Path $ToolTemp -Destination $InstalledToolsBinaryPath
            $ToolGoInstalled = $true
            & $CleanupStaleUpdater
        }
        else {
            Write-Error "Go urnet-tools digest mismatch (got $ActualHash); cannot install"
            Remove-Item -Path $ToolTemp -Force -ErrorAction SilentlyContinue
        }
    }
    catch {
        Write-Error "Failed to install Go urnet-tools: $($_.Exception.Message)"
        Remove-Item -Path $ToolTemp -Force -ErrorAction SilentlyContinue
    }
}

if (-not $ToolGoInstalled) {
    Write-Error "Failed to install Go urnet-tools binary. The provider requires urnet-tools.exe (Go binary) to manage lifecycle on Windows. Please download it manually from the release page."
    exit 1
}

if (-not $NoUTAlias -and $env:URNETWORK_NO_UT_ALIAS -ne "1") {
    if ($UTAliasOwned) { Copy-Item -LiteralPath $InstalledToolsBinaryPath -Destination $UTAliasPath -Force }
    else { Write-Warning "ut alias: $UTAliasPath already exists, leaving it unchanged" }
}

Set-Content $VersionFile $ReleaseVersion
Set-Content $InstallDateFile $ReleaseDate

if ($Version -eq "latest") {
    Write-Host "Running: urnet-tools auto-update weekly"
    & $InstalledToolsBinaryPath auto-update weekly
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

	if ($CurrentPath -match ";$") {
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
	# Use the Go tool's schtasks onlogon task for auto-start.
	if ($ToolGoInstalled -and (Test-Path $InstalledToolsBinaryPath)) {
	    Write-Host "Enabling auto-start via Task Scheduler (urnet-tools auto-start on)"
	    & $InstalledToolsBinaryPath auto-start on
	    if (-not $?) {
		Write-Warning "auto-start enable failed (exit $LASTEXITCODE)"
	    }
	}
    }
}
