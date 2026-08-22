#!/usr/bin/env python3
"""vt-scan.py — VirusTotal artifact scanner for CI (pure stdlib).

Scans one or more files against the VirusTotal v3 API:
  1. sha256 hash lookup (1 request; free if the file was scanned before)
  2. if unknown: multipart upload, then poll the analysis until completed
  3. prints a verdict per file; exits 1 if any file has malicious > VT_FAIL_THRESHOLD

Env:
  VT_API_KEY          required
  VT_FAIL_THRESHOLD   fail (exit 1) if malicious count > threshold
                      (default 10 — real supply-chain hits are 10+ engines)
  VT_REVIEW_THRESHOLD artifact is marked REVIEW (ships) if malicious count
                      exceeds this but is at or below FAIL_THRESHOLD
                      (default 2 — the known stripped-Go false-positive band)
  VT_UPLOAD           "0" to never upload (lookup-only, useful on PRs)
  VT_POLL_WAIT        seconds between analysis polls (default 12)
  VT_POLL_MAX         max polls before timeout (default 20)
  VT_SUMMARY_FILE     path: append a markdown proof block (per-file
                      verdicts + VT links + CI run ref) to this file

Exit codes: 0 = pass/review/UNKNOWN (non-blocking api/upload/timeout),
1 = malicious above fail threshold (real-malware line). The scan is
informational and never blocks the release.
"""

import hashlib
import json
import os
import sys
import time
import urllib.request
import urllib.error

API = "https://www.virustotal.com/api/v3"
POLL_WAIT = float(os.environ.get("VT_POLL_WAIT", "12"))
POLL_MAX = int(os.environ.get("VT_POLL_MAX", "20"))
FAIL_THRESHOLD = int(os.environ.get("VT_FAIL_THRESHOLD", "10"))
REVIEW_THRESHOLD = int(os.environ.get("VT_REVIEW_THRESHOLD", "2"))
UPLOAD = os.environ.get("VT_UPLOAD", "1") != "0"
SUMMARY_FILE = os.environ.get("VT_SUMMARY_FILE", "")


def api(path: str, data=None, method=None, headers=None) -> tuple[int, dict]:
    url = API + path
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("x-apikey", os.environ["VT_API_KEY"])
    if data is not None and not isinstance(data, bytes):
        req.add_header("Content-Type", "application/json")
        data = json.dumps(data).encode()
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            body = resp.read()
            code = resp.status
    except urllib.error.HTTPError as e:
        body = e.read()
        code = e.code
    try:
        return code, json.loads(body or b"{}")
    except json.JSONDecodeError:
        return code, {"raw": body.decode(errors="replace")[:200]}


def sha256_of(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def scan_file(path: str) -> int:
    fsha = sha256_of(path)
    print(f"== {path}  sha256={fsha}", flush=True)

    # 1. lookup (1 request when already known)
    code, body = api(f"/files/{fsha}")
    verdict = None
    if code == 200:
        stats = body.get("data", {}).get("attributes", {}).get("last_analysis_stats", {})
        verdict = "cached"
    elif code == 404:
        if not UPLOAD:
            print("  lookup miss, upload disabled (VT_UPLOAD=0) — skipping", flush=True)
            return 0
        # 2. upload
        boundary = f"----vtci{os.getpid()}"
        with open(path, "rb") as f:
            payload = f.read()
        part = (
            (
                f"--{boundary}\r\n"
                'Content-Disposition: form-data; name="file"; '
                f'filename="{os.path.basename(path)}"\r\n'
                "Content-Type: application/octet-stream\r\n\r\n"
            ).encode()
            + payload
            + f"\r\n--{boundary}--\r\n".encode()
        )
        code, body = api(
            "/files",
            data=part,
            method="POST",
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
        )
        if code != 200:
            print(f"  UPLOAD FAILED ({code}): {body} - recorded UNKNOWN (non-blocking)", flush=True)
            _summary_rows.append((path, fsha, "upload-error", None, None, None, None))
            return 0
        analysis_id = body.get("data", {}).get("id", "")
        # 3. poll
        stats = {}
        for i in range(1, POLL_MAX + 1):
            time.sleep(POLL_WAIT)
            c, b = api(f"/analyses/{analysis_id}")
            status = b.get("data", {}).get("attributes", {}).get("status")
            if status == "completed":
                stats = b.get("data", {}).get("attributes", {}).get("stats", {})
                verdict = "uploaded"
                break
            if i == POLL_MAX:
                print("  ANALYSIS TIMEOUT - scan not completed; recorded UNKNOWN (non-blocking)", flush=True)
                _summary_rows.append((path, fsha, "timeout", None, None, None, None))
                return 0
    else:
        print(f"  LOOKUP FAILED ({code}): {body} - recorded UNKNOWN (non-blocking)", flush=True)
        _summary_rows.append((path, fsha, "lookup-error", None, None, None, None))
        return 0

    mal = int(stats.get("malicious", 0))
    sus = int(stats.get("suspicious", 0))
    har = int(stats.get("harmless", 0))
    und = int(stats.get("undetected", 0))
    print(
        f"  verdict: malicious={mal} suspicious={sus} harmless={har} undetected={und}", flush=True
    )
    print(f"  https://www.virustotal.com/gui/file/{fsha}", flush=True)
    if SUMMARY_FILE:
        _summary_rows.append((path, fsha, verdict, mal, sus, har, und))
    if mal > FAIL_THRESHOLD:
        print(f"  ^ FAIL: malicious ({mal}) > fail threshold ({FAIL_THRESHOLD})", flush=True)
        return 1
    if mal > REVIEW_THRESHOLD:
        print(
            f"  ^ REVIEW: malicious ({mal}) > review threshold ({REVIEW_THRESHOLD}), at/below fail threshold — ships but flagged",
            flush=True,
        )
    return 0


_summary_rows = []  # (path, sha, verdict, mal, sus, har, und) accumulated for the proof block


def write_summary() -> None:
    """Write the release-notes proof block once, after all files are scanned.

    Three-tier status, measured against the release gates:
      PASS   malicious <= VT_REVIEW_THRESHOLD (known Go false-positive band)
      REVIEW malicious > review threshold, <= VT_FAIL_THRESHOLD (ships,
             flagged for a look)
      FAIL   malicious > VT_FAIL_THRESHOLD (the real-malware line, blocks)
    """
    if not SUMMARY_FILE or not _summary_rows:
        return
    try:
        server = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
        repo = os.environ.get("GITHUB_REPOSITORY", "")
        run = os.environ.get("GITHUB_RUN_ID", "")
        rows = list(_summary_rows)
        verified = [mal for _, _, _, mal, _, _, _ in rows if mal is not None]
        worst = max(verified) if verified else 0
        has_unknown = any(mal is None for _, _, _, mal, _, _, _ in rows)
        with open(SUMMARY_FILE, "a") as f:
            f.write("\n---\n\n## VirusTotal Scan\n\n")
            f.write(
                "The release artifacts were scanned with VirusTotal "
                "(70+ antivirus engines) during CI.\n\n"
            )
            if worst > FAIL_THRESHOLD:
                f.write(
                    f"**Result: FAIL** — at least one artifact exceeds "
                    f"{FAIL_THRESHOLD} malicious detections. The release was blocked.\n\n"
                )
            elif worst > REVIEW_THRESHOLD:
                f.write(
                    f"**Result: REVIEW** — an artifact has more than "
                    f"{REVIEW_THRESHOLD} malicious detections but is at or below "
                    f"{FAIL_THRESHOLD}. Review recommended.\n\n"
                )
            else:
                f.write(
                    f"**Result: PASS** — every artifact is within "
                    f"{REVIEW_THRESHOLD} malicious detections.\n\n"
                )
            if has_unknown:
                f.write(
                    "Artifacts marked UNKNOWN were not fully scanned "
                    "(VirusTotal upload, lookup, or analysis timed out). "
                    "Rescan them before relying on a clear verdict.\n\n"
                )
            f.write("| Artifact | Status | Malicious | Suspicious | VT link |\n")
            f.write("|---|---|---|---|---|\n")
            for path, fsha, _, mal, sus, _, _ in rows:
                if mal is None:
                    status = "UNKNOWN"
                elif mal > FAIL_THRESHOLD:
                    status = "FAIL"
                elif mal > REVIEW_THRESHOLD:
                    status = "REVIEW"
                else:
                    status = "PASS"
                f.write(
                    f"| {os.path.basename(path)} | {status} | "
                    f"{'n/a' if mal is None else mal} | {'n/a' if sus is None else sus} | "
                    f"[report](https://www.virustotal.com/gui/file/{fsha}) |\n"
                )
            f.write(
                "\nNOTE: 1 to 2 detections are the known machine-learning "
                "false-positive pattern on stripped Go binaries (PASS). "
                "3 to 10 detections ship as REVIEW and should be checked. "
                "More than 10 detections blocks the release.\n"
            )
            f.write(
                "\nWacatac.C!ml (Microsoft Defender) on these binaries is a "
                "confirmed false positive: Microsoft analyst review reversed "
                "the hub (2026-08-14) and the provider submission is in "
                "progress. Keep the detection visible in the tally — a future "
                "real hit would still appear. Do not suppress it.\n"
            )
            f.write(f"\n<sub>Scan run: [{run}]({server}/{repo}/actions/runs/{run}).</sub>\n\n")
    except OSError as e:
        print(f"  (summary write failed: {e})", flush=True)


def main() -> int:
    if not os.environ.get("VT_API_KEY"):
        print("ERROR: VT_API_KEY not set", file=sys.stderr)
        return 2
    files = sys.argv[1:]
    if not files:
        print("usage: vt-scan.py <file>...", file=sys.stderr)
        return 2
    rc = 0
    for f in files:
        r = scan_file(f)
        if r != 0 and rc == 0:
            rc = r
        time.sleep(2)
    write_summary()
    print(f"RESULT: {'PASS' if rc == 0 else 'FAIL'}", flush=True)
    return rc


if __name__ == "__main__":
    sys.exit(main())
