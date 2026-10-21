#!/usr/bin/env python3
"""Tests for .github/scripts/vt-scan.py against a local stub of the v3 API.

Run: python3 .github/scripts/test_vt_scan.py
No network, no API key, no quota: the stub below answers /files, /files/upload_url,
the large-file PUT, and /analyses.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "vt-scan.py")


class Stub:
    """Mutable behaviour for the fake VirusTotal API."""

    def __init__(self):
        self.files = {}                    # sha -> stats (present == already known to VT)
        self.unknown_stats = {"malicious": 0, "suspicious": 0, "harmless": 0,
                              "undetected": 60, "type-unsupported": 5}
        self.analysis = {}                 # analysis id -> sha
        self.upload_fail = False           # POST /files returns 500
        self.throttle = 0                  # 429s to serve before succeeding, per lookup
        self.polls_to_complete = 1
        self.delay_upload_url = 0          # 429s before /files/upload_url succeeds
        self.requests = []                 # (method, path) in order
        self.small_posts = 0
        self.large_posts = 0
        self.fail_upload_posts = 0   # 500s to serve on the upload-url POST, then succeed


class Handler(BaseHTTPRequestHandler):
    stub = None

    def log_message(self, *a):
        pass

    def _send(self, code, obj, headers=None):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _record(self, method):
        self.stub.requests.append((method, self.path.split("?")[0]))

    def do_GET(self):
        self._record("GET")
        s = self.stub
        path = self.path.split("?")[0]
        if path == "/files/upload_url":
            if s.delay_upload_url > 0:
                s.delay_upload_url -= 1
                return self._send(429, {"error": {"code": "QuotaExceededError"}},
                                  {"Retry-After": "1"})
            return self._send(200, {"data": f"http://127.0.0.1:{self.server.server_port}/upload/large"})
        if path.startswith("/files/"):
            sha = path[len("/files/"):]
            if s.throttle > 0:
                s.throttle -= 1
                return self._send(429, {"error": {"code": "QuotaExceededError"}},
                                  {"Retry-After": "1"})
            if sha in s.files:
                return self._send(200, {"data": {"attributes": {
                    "last_analysis_stats": s.files[sha]}}})
            return self._send(404, {"error": {"code": "NotFoundError"}})
        if path.startswith("/analyses/"):
            aid = path[len("/analyses/"):]
            sha = s.analysis.get(aid)
            done = getattr(self.server, "poll_counts", {}).get(aid, 0) + 1
            self.server.poll_counts = getattr(self.server, "poll_counts", {})
            self.server.poll_counts[aid] = done
            if done <= s.polls_to_complete and s.polls_to_complete:
                return self._send(200, {"data": {"attributes": {"status": "queued"}}})
            return self._send(200, {"data": {"attributes": {
                "status": "completed", "stats": s.files.get(sha, {})}}})
        return self._send(404, {"error": {"code": "NotFoundError"}})

    def _register(self, content):
        """Pretend VT finished scanning this content; returns an analysis id."""
        s = self.stub
        sha = hashlib.sha256(content).hexdigest()
        s.files.setdefault(sha, s.unknown_stats)
        aid = f"an-{len(s.analysis) + 1}"
        s.analysis[aid] = sha
        return aid

    def do_POST(self):
        self._record("POST")
        s = self.stub
        path = self.path.split("?")[0]
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        if path in ("/files", "/upload/large"):
            if s.upload_fail:
                return self._send(500, {"error": {"code": "InternalError"}})
            content = raw.split(b"\r\n\r\n", 1)[1].rsplit(b"\r\n--", 1)[0] if b"\r\n\r\n" in raw else raw
            if path == "/files":
                s.small_posts += 1
            else:
                if s.fail_upload_posts > 0:
                    s.fail_upload_posts -= 1
                    return self._send(500, {"error": {"code": "InternalError"}})
                s.large_posts += 1
            return self._send(200, {"data": {"id": self._register(content)}})
        return self._send(404, {"error": {"code": "NotFoundError"}})

    def do_PUT(self):
        # A raw PUT is what the real API rejects with 400 'Argument "file" is
        # missing'; keep it failing so a regression cannot pass silently.
        self._record("PUT")
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        return self._send(400, {"error": {"code": "BadRequestError",
                                          "message": 'Argument "file" is missing'}})


class VTScanTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.server.daemon_threads = True
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        self.stub = Stub()
        Handler.stub = self.stub
        self.server.poll_counts = {}
        self.tmp = tempfile.mkdtemp(prefix="vt-test-")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    # helpers -----------------------------------------------------------
    def make_file(self, name, size=64, fill=b"a"):
        path = os.path.join(self.tmp, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(fill * size)
        return path

    def run_scan(self, paths, **env_over):
        env = {
            "PATH": os.environ.get("PATH", ""),
            "VT_API_KEY": "test-key",
            "VT_API_BASE": f"http://127.0.0.1:{self.server.server_port}",
            "VT_POLL_WAIT": "0",
            "VT_POLL_MAX": "5",
            "VT_RETRIES": "2",
            "VT_MIN_INTERVAL": "0",
            "VT_HTTP_TIMEOUT": "10",
            "VT_SUMMARY_FILE": os.path.join(self.tmp, "summary.md"),
            "VT_JSON_FILE": os.path.join(self.tmp, "results.json"),
        }
        env.update({k: str(v) for k, v in env_over.items()})
        proc = subprocess.run([sys.executable, SCRIPT] + paths, env=env,
                              capture_output=True, text=True, timeout=120)
        rows = None
        jpath = env["VT_JSON_FILE"]
        if os.path.exists(jpath):
            with open(jpath) as f:
                rows = json.load(f)
        summary = ""
        spath = env["VT_SUMMARY_FILE"]
        if os.path.exists(spath):
            with open(spath) as f:
                summary = f.read()
        return proc, rows, summary

    # tests -------------------------------------------------------------
    def test_cached_known_file_is_pass_without_upload(self):
        p = self.make_file("release_tmp/linux/amd64/provider", 128)
        import hashlib
        sha = hashlib.sha256(b"a" * 128).hexdigest()
        self.stub.files[sha] = {"malicious": 1, "suspicious": 0, "harmless": 0,
                                "undetected": 63, "type-unsupported": 6}
        proc, rows, summary = self.run_scan([p])
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertEqual(self.stub.small_posts, 0)
        self.assertEqual(rows[0]["verdict"], "PASS")
        self.assertEqual(rows[0]["source"], "cached")
        self.assertEqual(rows[0]["engines"], 70)
        self.assertEqual(rows[0]["label"], "linux/amd64/provider")
        self.assertIn("**Result: PASS**", summary)

    def test_unknown_small_file_uploads_and_scans(self):
        p = self.make_file("release_tmp/linux/arm64/urnet-tools", 256)
        self.stub.unknown_stats = {"malicious": 0, "suspicious": 0, "harmless": 0,
                                   "undetected": 65}
        proc, rows, summary = self.run_scan([p])
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertEqual(self.stub.small_posts, 1)
        self.assertEqual(rows[0]["source"], "uploaded")
        self.assertEqual(rows[0]["verdict"], "PASS")
        self.assertIn("| linux/arm64/urnet-tools | PASS |", summary)

    def test_large_file_uses_upload_url_flow(self):
        p = self.make_file("release_tmp/windows/amd64/provider", 200)
        proc, rows, summary = self.run_scan([p], VT_SMALL_MAX=100)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        urls = [m for m, path in self.stub.requests if path == "/files/upload_url"]
        self.assertEqual(urls, ["GET"])
        self.assertEqual(self.stub.large_posts, 1, self.stub.requests)
        self.assertEqual(self.stub.small_posts, 0)
        self.assertNotIn("PUT", [m for m, _ in self.stub.requests], "upload URL takes a POST, not a PUT")
        self.assertEqual(rows[0]["verdict"], "PASS")

    def test_429_on_lookup_is_retried(self):
        p = self.make_file("release_tmp/darwin/arm64/provider", 64)
        import hashlib
        sha = hashlib.sha256(b"a" * 64).hexdigest()
        self.stub.files[sha] = {"malicious": 0, "suspicious": 0, "harmless": 0, "undetected": 60}
        self.stub.throttle = 1
        proc, rows, _ = self.run_scan([p])
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        lookups = [1 for m, path in self.stub.requests if path.startswith("/files/") and path != "/files/upload_url"]
        self.assertEqual(lookups, [1, 1])
        self.assertIn("429", proc.stdout)
        self.assertEqual(rows[0]["verdict"], "PASS")

    def test_fail_threshold_exits_1(self):
        p = self.make_file("release_tmp/linux/amd64/provider", 32)
        import hashlib
        sha = hashlib.sha256(b"a" * 32).hexdigest()
        self.stub.files[sha] = {"malicious": 11, "suspicious": 0, "harmless": 0, "undetected": 50}
        proc, rows, summary = self.run_scan([p], VT_FAIL_THRESHOLD=10, VT_REVIEW_THRESHOLD=2)
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(rows[0]["verdict"], "FAIL")
        self.assertIn("**Result: FAIL**", summary)

    def test_review_band_ships_with_exit_0(self):
        p = self.make_file("release_tmp/linux/amd64/urnet-docker", 32)
        import hashlib
        sha = hashlib.sha256(b"a" * 32).hexdigest()
        self.stub.files[sha] = {"malicious": 5, "suspicious": 0, "harmless": 0, "undetected": 55}
        proc, rows, summary = self.run_scan([p], VT_FAIL_THRESHOLD=10, VT_REVIEW_THRESHOLD=2)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(rows[0]["verdict"], "REVIEW")
        self.assertIn("**Result: REVIEW**", summary)

    def test_upload_failure_is_reported_as_incomplete(self):
        p = self.make_file("release_tmp/linux/amd64/provider", 64)
        self.stub.upload_fail = True
        proc, rows, summary = self.run_scan([p])
        self.assertEqual(proc.returncode, 0, "unscanned artifact must not fail the release by default")
        self.assertIsNone(rows[0]["malicious"])
        self.assertEqual(rows[0]["verdict"], "UNKNOWN")
        self.assertTrue(rows[0]["error"])
        self.assertEqual(rows[0]["path"], p)
        self.assertIn("sha", rows[0])
        self.assertIn("**Result: INCOMPLETE**", summary)
        self.assertNotIn("**Result: PASS**", summary)

    def test_incomplete_strict_exits_2(self):
        p = self.make_file("release_tmp/linux/amd64/provider", 64)
        self.stub.upload_fail = True
        proc, _, _ = self.run_scan([p], VT_FAIL_ON_INCOMPLETE=1)
        self.assertEqual(proc.returncode, 2)

    def test_identical_files_scanned_once(self):
        a = self.make_file("release_tmp/windows/amd64/provider", 48)
        b = self.make_file("release_tmp/windows/amd64/provider.exe", 48)
        import hashlib
        sha = hashlib.sha256(b"a" * 48).hexdigest()
        self.stub.files[sha] = {"malicious": 0, "suspicious": 0, "harmless": 0, "undetected": 60}
        proc, rows, summary = self.run_scan([a, b])
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(len(rows), 2)
        dup = [r for r in rows if r["duplicate_of"]]
        self.assertEqual(len(dup), 1)
        self.assertEqual(dup[0]["duplicate_of"], "windows/amd64/provider")
        self.assertEqual(dup[0]["source"], "duplicate")
        self.assertEqual(dup[0]["malicious"], 0, "duplicate carries the source verdict")
        lookups = [1 for m, path in self.stub.requests if path.startswith("/files/") and path != "/files/upload_url"]
        self.assertEqual(len(lookups), 1)
        self.assertIn("Identical bytes", summary)
        self.assertIn("**Result: PASS**", summary, "a duplicate must not make the run INCOMPLETE")
        self.assertNotIn("INCOMPLETE", summary)

    def test_upload_disabled_marks_unscanned(self):
        p = self.make_file("release_tmp/linux/amd64/provider", 32)
        proc, rows, summary = self.run_scan([p], VT_UPLOAD=0)
        self.assertEqual(proc.returncode, 0)
        self.assertIsNone(rows[0]["malicious"])
        self.assertIn("INCOMPLETE", summary)

    def test_missing_file_is_reported(self):
        proc, rows, summary = self.run_scan([os.path.join(self.tmp, "nope")])
        self.assertIsNone(rows[0]["malicious"])
        self.assertEqual(rows[0]["verdict"], "UNKNOWN")
        self.assertIn("missing file", rows[0]["error"])
        self.assertIn("INCOMPLETE", summary)

    def test_polls_do_not_run_between_uploads(self):
        """Both uploads must be submitted before the first poll."""
        a = self.make_file("release_tmp/linux/amd64/provider", 33, b"x")
        b = self.make_file("release_tmp/linux/arm64/provider", 34, b"y")
        proc, rows, _ = self.run_scan([a, b])
        seq = [(m, p) for m, p in self.stub.requests]
        posts = [i for i, (m, p) in enumerate(seq) if p == "/files"]
        polls = [i for i, (m, p) in enumerate(seq) if p.startswith("/analyses/")]
        self.assertEqual(len(posts), 2, seq)
        self.assertTrue(polls, seq)
        self.assertLess(max(posts), min(polls), seq)
        self.assertEqual([r["verdict"] for r in rows], ["PASS", "PASS"])

    def test_large_upload_fetches_fresh_url_after_failure(self):
        """The upload URL is single-use: a failed POST must not be replayed."""
        p = self.make_file("release_tmp/darwin/amd64/provider", 200)
        self.stub.fail_upload_posts = 1
        proc, rows, _ = self.run_scan([p], VT_SMALL_MAX=100)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        urls = [1 for m, path in self.stub.requests if path == "/files/upload_url"]
        self.assertEqual(len(urls), 2, self.stub.requests)
        self.assertEqual(self.stub.large_posts, 1)
        self.assertEqual(rows[0]["verdict"], "PASS")

    def test_incomplete_headline_names_review_detections(self):
        """An unscanned file plus a REVIEW artifact must not be called clean."""
        import hashlib
        ok = self.make_file("release_tmp/linux/amd64/urnet-docker", 40, b"r")
        bad = self.make_file("release_tmp/linux/arm64/urnet-docker", 41, b"q")
        sha = hashlib.sha256(b"r" * 40).hexdigest()
        self.stub.files[sha] = {"malicious": 5, "suspicious": 0, "harmless": 0, "undetected": 55}
        self.stub.upload_fail = True
        proc, rows, summary = self.run_scan([ok, bad], VT_FAIL_THRESHOLD=10, VT_REVIEW_THRESHOLD=2)
        self.assertEqual(proc.returncode, 0)
        self.assertIn("**Result: INCOMPLETE**", summary)
        self.assertIn("includes REVIEW-level detections", summary)
        self.assertNotIn("is clean", summary)

    def test_upload_url_429_is_retried(self):
        p = self.make_file("release_tmp/windows/arm64/provider", 200)
        self.stub.delay_upload_url = 1
        proc, rows, _ = self.run_scan([p], VT_SMALL_MAX=100)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertEqual(rows[0]["verdict"], "PASS")


if __name__ == "__main__":
    unittest.main(verbosity=2)
