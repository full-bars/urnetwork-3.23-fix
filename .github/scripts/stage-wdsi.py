#!/usr/bin/env python3
"""
stage-wdsi.py — Automate Microsoft WDSI false-positive submission preparation.

Runs as a post-scan step in CI after vt-scan.py. For each artifact with
malicious > 0, it:
  1. Queries the VT API for Microsoft's detection name + engine version
  2. Copies the local binary (from the already-downloaded scan artifacts)
  3. Verifies SHA256 against the VT record
  4. Generates a deterministic WDSI submission text block with per-file details
  5. Bundles everything into defender-submissions-<tag>/ with WDSI-submission-text.txt

Usage:
  python3 stage-wdsi.py <release-tag> <vt-results-json> <source-dir>

Env:
  VIRUS_TOTAL  — VT API key

Output:
  defender-submissions-<tag>/
    <flagged binaries...>
    WDSI-submission-text.txt
"""
import hashlib
import json
import os
import re
import shutil
import sys
import urllib.request

VT_API = "https://www.virustotal.com/api/v3"


def vt_api(path, api_key):
    """Make an authenticated request to the VirusTotal v3 API."""
    req = urllib.request.Request(VT_API + path)
    req.add_header("x-apikey", api_key)
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.loads(resp.read())


def sha256_of(path):
    """Compute the SHA256 hex digest of a file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def scan_path_to_asset(path):
    """Map a VT scan path (e.g. release_tmp/linux/amd64/provider) to a
    human-readable artifact name for the WDSI form and the destination dir."""
    basename = os.path.basename(path)
    # Strip any extension to avoid double-extension issues (e.g. provider.exe)
    basename_stripped = os.path.splitext(basename)[0]
    # Build a descriptive name with platform
    parts = path.split(os.sep)
    platform = ""
    if "darwin" in parts:
        platform = "macOS"
    elif "linux" in parts:
        platform = "Linux"
    elif "windows" in parts:
        platform = "Windows"
    elif "amd64" in parts or "arm64" in parts:
        platform = "Linux"
    arch = ""
    if "arm64" in parts:
        arch = "arm64"
    elif "amd64" in parts:
        arch = "amd64"

    # Special names for display — match against extension-stripped basename
    if basename_stripped == "provider":
        display = "urnetwork-provider"
    elif basename_stripped == "urnet-tools":
        display = "urnet-tools"
    elif basename_stripped == "urnet-docker":
        display = "urnet-docker"
    else:
        display = basename_stripped

    ext = ".exe" if platform == "Windows" else ""
    return f"{display}-{platform.lower()}-{arch}{ext}", basename_stripped, platform, arch


def main():
    """Stage flagged binaries and WDSI submission text for a release tag."""
    if len(sys.argv) < 4:
        print("usage: stage-wdsi.py <release-tag> <vt-results-json> <source-dir>", file=sys.stderr)
        return 2

    tag = sys.argv[1]
    vt_json_path = sys.argv[2]
    source_dir = sys.argv[3]  # e.g. release_tmp/ — already downloaded by scan job

    vt_key = os.environ.get("VIRUS_TOTAL", "")

    if not vt_key:
        print("ERROR: VIRUS_TOTAL not set", file=sys.stderr)
        return 2

    # Read VT scan results
    with open(vt_json_path) as f:
        vt_results = json.load(f)

    # Filter to files with Microsoft detections (malicious > 0)
    flagged = [r for r in vt_results if (r.get("malicious") or 0) > 0]
    if not flagged:
        print("No flagged files — nothing to stage")
        return 0

    # Sanitize tag for use as a directory name
    safe_tag = re.sub(r'[^a-zA-Z0-9._-]', '_', tag).removeprefix('v')
    dest_dir = f"defender-submissions-{safe_tag}"
    if os.path.exists(dest_dir):
        shutil.rmtree(dest_dir)
    os.makedirs(dest_dir)

    # Determine the repository URL for WDSI text
    repo = os.environ.get("GITHUB_REPOSITORY", "full-bars/urnetwork-3.23-fix")
    repo_url = f"https://github.com/{repo}"

    # First pass: gather detection details from VT for each flagged file
    file_details = []
    for row in flagged:
        sha = row["sha"]
        scan_path = row["path"]
        mal = row["malicious"]
        asset_name, basename, platform, arch = scan_path_to_asset(scan_path)

        # Query VT for engine-specific details
        detection_name = "Unknown"
        engine_version = ""
        try:
            result = vt_api(f"/files/{sha}", vt_key)
            last_results = result.get("data", {}).get("attributes", {}).get("last_analysis_results", {})
            msft = last_results.get("Microsoft", {})
            detection_name = msft.get("result") or "Unknown"
            engine_version = msft.get("engine_version", "")
            msft_category = msft.get("category", "")
            # Skip files that Microsoft Defender didn't actually flag —
            # aggregate malicious > 0 may come from other AV engines
            if msft_category != "malicious" and not msft.get("result"):
                print(f"  SKIP: {asset_name} — clean in Defender (other AVs flagged)")
                continue
        except Exception as e:
            print(f"  WARNING: VT lookup for {sha} failed: {e}", file=sys.stderr)

        # Find the local file — scan job organizes by tool category
        local_path = None
        basename_scan = scan_path  # e.g. release_tmp/linux/amd64/provider
        # Try the exact path first (release_tmp subdirs)
        candidate = os.path.join(source_dir, basename_scan)
        if not os.path.isfile(candidate):
            # Try just the basename in source_dir
            candidate = os.path.join(source_dir, os.path.basename(basename_scan))
        if os.path.isfile(candidate):
            local_path = candidate
        else:
            # Hubs and tools are in hub_tmp/ and tool_tmp/ respectively
            parts = basename_scan.split(os.sep)
            plat_parts = [p for p in parts if p in ("linux", "darwin", "windows", "amd64", "arm64")]
            file_base = os.path.basename(basename_scan)
            for prefix in ["hub_tmp", "tool_tmp"]:
                sub = os.path.join(prefix, *plat_parts) if plat_parts else prefix
                candidate2 = os.path.join(source_dir, sub, file_base)
                if os.path.isfile(candidate2):
                    local_path = candidate2
                    break
            if local_path is None:
                print(f"  WARNING: local file not found for {scan_path}", file=sys.stderr)

        file_details.append({
            "sha": sha,
            "malicious": mal,
            "detection_name": detection_name,
            "engine_version": engine_version,
            "asset_name": asset_name,
            "basename": basename,
            "platform": platform,
            "arch": arch,
            "local_path": local_path,
        })
        print(f"  {asset_name}: {detection_name} (engine: {engine_version}, SHA: {sha[:16]}...)")

    # Second pass: copy flagged binaries from local source, filter out failures
    verified_details = []
    for fd in file_details:
        if not fd["local_path"]:
            print(f"  SKIP: no local file for {fd['asset_name']}", file=sys.stderr)
            continue

        dest_file = os.path.join(dest_dir, fd["asset_name"])
        shutil.copy2(fd["local_path"], dest_file)
        os.chmod(dest_file, 0o755)

        # Verify SHA256
        actual_sha = sha256_of(dest_file)
        if actual_sha != fd["sha"]:
            print(f"  SHA MISMATCH for {fd['asset_name']}! expected {fd['sha']}, got {actual_sha}", file=sys.stderr)
            os.remove(dest_file)
            continue
        print(f"  Copied {fd['asset_name']} (SHA256 verified)")
        verified_details.append(fd)
    file_details = verified_details

    # Third pass: generate WDSI submission text (deterministic template)
    print("  Generating WDSI submission text...")
    lines = [
        f"=== Microsoft WDSI False-Positive Submission — {tag} ===",
        "",
        "Submission type: My file was incorrectly detected",
        "Product: Windows Defender",
    ]
    for fd in file_details:
        lines.extend([
            "---",
            "",
            f"File: {fd['asset_name']}",
            f"SHA256: {fd['sha']}",
            f"Malicious: {fd['malicious']}",
            f"VT: https://www.virustotal.com/gui/file/{fd['sha']}",
            f"Detection: {fd['detection_name']}",
            f"Engine: {fd['engine_version']}",
            "",
            "Additional info:",
            f"Open-source Go binary (MPL-2.0) from "
            f"{repo_url} — "
            f"{fd['asset_name']} ({fd['platform']} {fd['arch']}). "
            f"Built with -trimpath -s -w. {fd['detection_name']} is a known "
            f"ML false positive on stripped Go binaries.",
            "",
        ])
    lines.extend([
        "---",
        "",
        "WDSI submission form: https://www.microsoft.com/en-us/wdsi/filesubmission",
        "One file per submission.",
        f"Bundle location: {dest_dir}/",
    ])
    with open(os.path.join(dest_dir, "WDSI-submission-text.txt"), "w") as f:
        f.write("\n".join(lines) + "\n")

    print(f"\nDone. Staged {len(file_details)} files in {dest_dir}/")
    print(f"WDSI text: {dest_dir}/WDSI-submission-text.txt")


if __name__ == "__main__":
    sys.exit(main())
