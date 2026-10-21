#!/usr/bin/env python3
"""vt-scan.py — VirusTotal artifact scanner for CI (pure stdlib).

Scans one or more files against the VirusTotal v3 API:
  1. sha256 hash lookup (1 request; free when the file was scanned before)
  2. if unknown: upload, then poll the analysis until completed
  3. prints a verdict per file and a markdown proof block for the release body

Why it looks like this — each rule fixes a failure seen in a real release run:
  * Free-tier keys allow 4 requests/minute. The old loop had no 429 handling,
    so a throttled call became an UNKNOWN row. Every request now honours
    Retry-After with exponential backoff.
  * The provider binaries sit at VirusTotal's 32 MiB single-POST limit and one
    of them tips over; that upload failed with 413 and shipped as UNKNOWN.
    Files over the limit now use the large-file flow (/files/upload_url + PUT).
  * Rescanning unchanged bytes is free, so every file is looked up by hash
    first, and identical files in one run are scanned once.
  * Uploads are submitted before analyses are polled: on a 4/minute budget,
    polling between uploads spends the quota on waiting instead of new work.
  * Coverage is explicit. A body that says PASS while an artifact was never
    scanned is worse than one that says INCOMPLETE.

Env:
  VT_API_KEY            required
  VT_API_BASE           API root (default https://www.virustotal.com/api/v3)
  VT_FAIL_THRESHOLD     fail above this many malicious detections (default 10 —
                        real supply-chain hits are 10+ engines)
  VT_REVIEW_THRESHOLD   REVIEW (ships, flagged) above this, at or below FAIL
                        (default 2 — the known stripped-Go false-positive band)
  VT_UPLOAD             "0" to never upload (lookup-only, useful on PRs)
  VT_POLL_WAIT          seconds between analysis polls (default 12)
  VT_POLL_MAX           max polls per analysis before a timeout (default 20)
  VT_RETRIES            retries on 429/5xx (default 5)
  VT_MIN_INTERVAL       minimum seconds between requests (default 0; use 16
                        with a free-tier key — 4 requests/minute)
  VT_SMALL_MAX          byte size above which the large-file flow is used
                        (default 33554432 = 32 MiB)
  VT_HTTP_TIMEOUT       per-request timeout in seconds (default 120)
  VT_SUMMARY_FILE       append the markdown proof block to this path
  VT_JSON_FILE          machine-readable results, one object per unique hash
  VT_FAIL_ON_INCOMPLETE "1" to exit non-zero when any artifact could not be
                        scanned (default "0": report it, keep the release green)

Exit codes: 0 = pass/review, 1 = malicious above VT_FAIL_THRESHOLD,
2 = coverage incomplete with VT_FAIL_ON_INCOMPLETE=1, or bad invocation.
"""

import datetime
import email.utils
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request

API = os.environ.get("VT_API_BASE", "https://www.virustotal.com/api/v3").rstrip("/")
POLL_WAIT = float(os.environ.get("VT_POLL_WAIT", "12"))
POLL_MAX = int(os.environ.get("VT_POLL_MAX", "20"))
FAIL_THRESHOLD = int(os.environ.get("VT_FAIL_THRESHOLD", "10"))
REVIEW_THRESHOLD = int(os.environ.get("VT_REVIEW_THRESHOLD", "2"))
UPLOAD = os.environ.get("VT_UPLOAD", "1") != "0"
RETRIES = int(os.environ.get("VT_RETRIES", "5"))
MIN_INTERVAL = float(os.environ.get("VT_MIN_INTERVAL", "0"))
SMALL_MAX = int(os.environ.get("VT_SMALL_MAX", str(32 * 1024 * 1024)))
HTTP_TIMEOUT = float(os.environ.get("VT_HTTP_TIMEOUT", "120"))
SUMMARY_FILE = os.environ.get("VT_SUMMARY_FILE", "")
JSON_FILE = os.environ.get("VT_JSON_FILE", "")
FAIL_ON_INCOMPLETE = os.environ.get("VT_FAIL_ON_INCOMPLETE", "0") == "1"

_last_call = 0.0


def _throttle() -> None:
    """Keep a minimum spacing between requests (free tier: 4/minute)."""
    global _last_call
    if MIN_INTERVAL <= 0:
        return
    delta = time.time() - _last_call
    if delta < MIN_INTERVAL:
        time.sleep(MIN_INTERVAL - delta)
    _last_call = time.time()


def _retry_after(err: urllib.error.HTTPError) -> float:
    """Seconds to wait from a Retry-After header.

    RFC 9110 allows either delay-seconds or an HTTP-date; VirusTotal uses both,
    and treating a date as "no header" burns quota on a backoff that is too short.
    """
    raw = err.headers.get("Retry-After") if err.headers else None
    if not isinstance(raw, str):
        return 0.0
    raw = raw.strip()
    try:
        return max(1.0, float(raw))
    except ValueError:
        pass
    try:
        when = email.utils.parsedate_to_datetime(raw)
    except (TypeError, ValueError):
        return 0.0
    if when is None:
        return 0.0
    if when.tzinfo is None:
        when = when.replace(tzinfo=datetime.timezone.utc)
    delta = (when - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
    return max(1.0, delta)


def call(url, data=None, method=None, headers=None, retries=None):
    """One API/upload request with 429 and 5xx backoff. Returns (code, dict).

    `retries=0` disables the internal retry loop, for requests that must not be
    replayed against the same URL (a single-use upload URL).
    """
    if not url.startswith("http"):
        url = API + url
    req_headers = {"x-apikey": os.environ["VT_API_KEY"]}
    req_headers.update(headers or {})
    payload = data
    if isinstance(data, dict):
        payload = json.dumps(data).encode()
        req_headers.setdefault("Content-Type", "application/json")
    attempts = RETRIES if retries is None else retries
    last = (0, {"raw": "not attempted"})
    for attempt in range(attempts + 1):
        _throttle()
        req = urllib.request.Request(url, data=payload, method=method)
        for k, v in req_headers.items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT) as resp:
                body = resp.read()
                try:
                    obj = json.loads(body or b"{}")
                except json.JSONDecodeError:
                    obj = {"raw": body[:300].decode(errors="replace")}
                return resp.status, obj if isinstance(obj, dict) else {"raw": obj}
        except urllib.error.HTTPError as e:
            body = e.read()
            try:
                obj = json.loads(body or b"{}")
                if not isinstance(obj, dict):
                    obj = {"raw": obj}
            except json.JSONDecodeError:
                obj = {"raw": body[:300].decode(errors="replace")}
            if (e.code == 429 or 500 <= e.code < 600) and attempt < attempts:
                wait = _retry_after(e) if e.code == 429 else 0.0
                wait = wait or min(60.0, 2.0 ** attempt)
                print(f"    {e.code} — retry {attempt + 1}/{RETRIES} in {wait:.0f}s", flush=True)
                time.sleep(wait)
                last = (e.code, obj)
                continue
            return e.code, obj
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            if attempt < attempts:
                wait = min(60.0, 2.0 ** attempt)
                print(f"    network error ({e}) — retry in {wait:.0f}s", flush=True)
                time.sleep(wait)
                last = (0, {"raw": str(e)})
                continue
            return 0, {"raw": str(e)}
    return last


def sha256_of(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def label_for(path: str) -> str:
    """`release_tmp/linux/amd64/provider` -> `linux/amd64/provider`."""
    parts = os.path.normpath(path).split(os.sep)
    if len(parts) >= 3 and parts[-3] in ("linux", "darwin", "windows", "freebsd"):
        return "/".join(parts[-3:])
    return os.path.basename(path)


class Row:
    __slots__ = ("path", "label", "sha", "size", "source", "analysis_id",
                 "malicious", "suspicious", "harmless", "undetected", "engines",
                 "error", "duplicate_of")

    path: str
    label: str
    sha: str
    size: int
    source: str | None
    analysis_id: str | None
    malicious: int | None
    suspicious: int | None
    harmless: int | None
    undetected: int | None
    engines: int | None
    error: str | None
    duplicate_of: str | None

    def __init__(self, path, sha, size, label):
        self.path = path
        self.label = label
        self.sha = sha
        self.size = size
        self.source = None
        self.analysis_id = None
        self.malicious = None
        self.suspicious = None
        self.harmless = None
        self.undetected = None
        self.engines = None
        self.error = None
        self.duplicate_of = None

    def set_stats(self, stats, source):
        self.source = source
        self.malicious = int(stats.get("malicious", 0))
        self.suspicious = int(stats.get("suspicious", 0))
        self.harmless = int(stats.get("harmless", 0))
        self.undetected = int(stats.get("undetected", 0))
        self.engines = sum(int(stats.get(k, 0)) for k in (
            "malicious", "suspicious", "harmless", "undetected",
            "timeout", "confirmed-timeout", "failure", "type-unsupported"))

    @property
    def scanned(self):
        return self.malicious is not None

    @property
    def verdict(self):
        if not self.scanned:
            return "UNKNOWN"
        if self.malicious > FAIL_THRESHOLD:
            return "FAIL"
        if self.malicious > REVIEW_THRESHOLD:
            return "REVIEW"
        return "PASS"


def write_json(rows):
    """Machine-readable results. `path`/`sha`/`malicious` are consumed by
    stage-wdsi.py, so they are present on every row, scanned or not."""
    if not JSON_FILE:
        return True
    out = [{
        "path": r.path,
        "sha": r.sha,
        "label": r.label,
        "bytes": r.size,
        "verdict": r.verdict,
        "source": r.source,
        "analysis_id": r.analysis_id,
        "malicious": r.malicious,
        "suspicious": r.suspicious,
        "harmless": r.harmless,
        "undetected": r.undetected,
        "engines": r.engines,
        "error": r.error,
        "duplicate_of": r.duplicate_of,
    } for r in rows]
    try:
        with open(JSON_FILE, "w") as f:
            json.dump(out, f, indent=2)
        return True
    except OSError as e:
        print(f"  (json write failed: {e})", flush=True)
        return False


def write_summary(rows):
    """Append the release-notes proof block.

    Headline order: FAIL beats everything; INCOMPLETE (any unscanned artifact)
    beats PASS/REVIEW, because a clean verdict for the scanned subset must not
    read as a clean verdict for the release.
    """
    if not SUMMARY_FILE or not rows:
        return
    verified = [r for r in rows if r.scanned]
    unknown = [r for r in rows if not r.scanned]
    worst = max((r.malicious for r in verified), default=0)
    try:
        with open(SUMMARY_FILE, "a") as f:
            f.write("\n---\n\n## VirusTotal Scan\n\n")
            f.write("The release artifacts were scanned with VirusTotal (60+ antivirus "
                    "engines) during CI.\n\n")
            if worst > FAIL_THRESHOLD:
                f.write(f"**Result: FAIL** — at least one artifact exceeds {FAIL_THRESHOLD} "
                        f"malicious detections. The release was blocked.\n\n")
            elif unknown:
                subset = ("includes REVIEW-level detections" if worst > REVIEW_THRESHOLD
                          else "is clean")
                f.write(f"**Result: INCOMPLETE** — {len(unknown)} of {len(rows)} artifacts "
                        f"could not be scanned (upload, lookup or analysis failure). The scanned "
                        f"subset of {len(verified)} {subset}. The unscanned artifacts are "
                        f"downloadable from this release; rescan them before relying on a clear "
                        f"verdict.\n\n")
            elif worst > REVIEW_THRESHOLD:
                f.write(f"**Result: REVIEW** — an artifact has more than {REVIEW_THRESHOLD} "
                        f"malicious detections but is at or below {FAIL_THRESHOLD}. Review "
                        f"recommended.\n\n")
            else:
                f.write(f"**Result: PASS** — all {len(verified)} artifacts are within "
                        f"{REVIEW_THRESHOLD} malicious detections.\n\n")
            f.write("| Artifact | Status | Malicious | Suspicious | Engines | VT link |\n")
            f.write("|---|---|---|---|---|---|\n")
            for r in rows:
                mal = "n/a" if r.malicious is None else r.malicious
                sus = "n/a" if r.suspicious is None else r.suspicious
                eng = "n/a" if r.engines is None else r.engines
                note = f" ({r.error})" if r.error else ""
                f.write(f"| {r.label}{note} | {r.verdict} | {mal} | {sus} | {eng} | "
                        f"[report](https://www.virustotal.com/gui/file/{r.sha}) |\n")
            dupes = [r for r in rows if r.duplicate_of]
            if dupes:
                f.write("\nIdentical bytes (scanned once): "
                        + ", ".join(f"{r.label} = {r.duplicate_of}" for r in dupes) + ".\n")
            f.write("\nNOTE: 1 to 2 detections are the known machine-learning false-positive "
                    "pattern on stripped Go binaries (PASS). 3 to 10 detections ship as REVIEW "
                    "and should be checked. More than 10 detections blocks the release.\n")
            f.write("\nHeuristic machine-learning detections (such as Wacatac or Execution.A!ml) "
                    "on these stripped Go binaries are known recurring false positives verified "
                    "clean upon Microsoft analyst dispute. Detections remain visible in the tally "
                    "to ensure transparency.\n")
            server = os.environ.get("GITHUB_SERVER_URL", "")
            repo = os.environ.get("GITHUB_REPOSITORY", "")
            run = os.environ.get("GITHUB_RUN_ID", "")
            if server and repo and run:
                f.write(f"\n<sub>Scan run: [{run}]({server}/{repo}/actions/runs/{run}).</sub>\n\n")
    except OSError as e:
        print(f"  (summary write failed: {e})", flush=True)


def multipart(path):
    """The multipart body VT expects for both /files and a large-file upload URL."""
    boundary = f"----vt{os.getpid()}"
    with open(path, "rb") as f:
        payload = f.read()
    part = ((f"--{boundary}\r\n"
             'Content-Disposition: form-data; name="file"; '
             f'filename="{os.path.basename(path)}"\r\n'
             "Content-Type: application/octet-stream\r\n\r\n").encode()
            + payload + f"\r\n--{boundary}--\r\n".encode())
    return part, {"Content-Type": f"multipart/form-data; boundary={boundary}"}


def submit(path):
    """Upload one file. Returns (analysis_id, error)."""
    part, mp_headers = multipart(path)
    if os.path.getsize(path) > SMALL_MAX:
        # The upload URL is single-use, so a retry has to start over with a
        # fresh URL: the POST runs with retries disabled and the whole
        # upload_url-then-POST sequence loops with backoff.
        code, body = 0, {"raw": "not attempted"}
        for attempt in range(RETRIES + 1):
            ucode, ubody = call("/files/upload_url")
            data = ubody.get("data")
            url = data.get("url") if isinstance(data, dict) else data
            if ucode != 200 or not isinstance(url, str) or not url:
                return None, f"upload_url failed ({ucode})"
            code, body = call(url, data=part, method="POST", headers=mp_headers, retries=0)
            if code == 200:
                break
            retryable = code == 0 or code == 429 or code >= 500
            if not retryable or attempt == RETRIES:
                break
            wait = min(60.0, 2.0 ** attempt)
            print(f"    upload failed ({code}), fetching a fresh upload url in {wait:.0f}s",
                  flush=True)
            time.sleep(wait)
    else:
        code, body = call("/files", data=part, method="POST", headers=mp_headers)
    if code != 200:
        err = body.get("error")
        detail = err.get("code", "") if isinstance(err, dict) else ""
        return None, f"upload failed ({code}{' ' + detail if detail else ''})"
    analysis_id = (body.get("data") or {}).get("id", "")
    if not analysis_id:
        return None, "upload returned no analysis id"
    return analysis_id, None


def poll(analysis_id, row):
    for i in range(1, POLL_MAX + 1):
        if POLL_WAIT:
            time.sleep(POLL_WAIT)
        code, body = call(f"/analyses/{analysis_id}")
        attrs = (body.get("data") or {}).get("attributes", {}) if code == 200 else {}
        if code == 200 and attrs.get("status") == "completed":
            row.set_stats(attrs.get("stats", {}), "uploaded")
            return
        if code not in (200, 404) and i == POLL_MAX:
            row.error = f"analysis poll failed ({code})"
            return
    row.error = f"analysis timeout after {POLL_MAX} polls"


def main():
    if not os.environ.get("VT_API_KEY"):
        print("ERROR: VT_API_KEY not set", file=sys.stderr)
        return 2
    paths = sys.argv[1:]
    if not paths:
        print("usage: vt-scan.py <file>...", file=sys.stderr)
        return 2

    rows = []
    pending = []
    by_sha = {}

    for p in paths:
        if not os.path.isfile(p):
            r = Row(p, "", 0, label_for(p))
            r.error = "missing file"
            rows.append(r)
            print(f"== {p}: MISSING", flush=True)
            continue
        fsha = sha256_of(p)
        if fsha in by_sha:
            first = by_sha[fsha]
            r = Row(p, fsha, os.path.getsize(p), label_for(p))
            r.duplicate_of = first.label
            rows.append(r)
            print(f"== {p}: identical to {first.label}, not rescanned", flush=True)
            continue
        r = Row(p, fsha, os.path.getsize(p), label_for(p))
        by_sha[fsha] = r
        rows.append(r)
        print(f"== {p} ({r.size}B) sha={fsha}", flush=True)

        code, body = call(f"/files/{fsha}")
        if code == 200:
            stats = (body.get("data") or {}).get("attributes", {}).get("last_analysis_stats", {})
            r.set_stats(stats, "cached")
            print(f"  cached: {r.verdict} malicious={r.malicious}", flush=True)
        elif code == 404:
            if UPLOAD:
                pending.append(r)
            else:
                r.error = "not on VirusTotal and VT_UPLOAD=0"
                print("  not on VirusTotal, uploads disabled", flush=True)
        else:
            r.error = f"lookup failed ({code})"
            print(f"  lookup failed ({code})", flush=True)

    # Submit every unknown file before polling any of them.
    for r in pending:
        r.analysis_id, err = submit(r.path)
        print(f"  {r.label}: uploaded as {r.analysis_id}" if not err
              else f"  {r.label}: {err}", flush=True)
        if err:
            r.error = err
            write_json(rows)

    for r in pending:
        if not r.analysis_id:
            continue
        poll(r.analysis_id, r)
        print(f"  {r.label}: {r.verdict}" if r.scanned
              else f"  {r.label}: {r.error or 'no verdict'}", flush=True)
        write_json(rows)

    # Identical bytes were deliberately not rescanned. Give the duplicate row
    # the verdict of the file it duplicates, otherwise a clean run reports
    # itself as INCOMPLETE just because two artifacts share content.
    for r in rows:
        if r.duplicate_of and not r.scanned:
            src = by_sha.get(r.sha)
            if src is not None and src.scanned:
                r.malicious = src.malicious
                r.suspicious = src.suspicious
                r.harmless = src.harmless
                r.undetected = src.undetected
                r.engines = src.engines
                r.source = "duplicate"
                r.error = None

    write_json(rows)
    write_summary(rows)
    fails = [r for r in rows if r.verdict == "FAIL"]
    unknown = [r for r in rows if not r.scanned]
    print(f"\nRESULT: {len(rows)} artifacts, {len(rows) - len(unknown)} scanned, "
          f"{len(fails)} fail, {len(unknown)} unscanned", flush=True)
    if fails:
        return 1
    if unknown and FAIL_ON_INCOMPLETE:
        print("coverage incomplete and VT_FAIL_ON_INCOMPLETE=1", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
