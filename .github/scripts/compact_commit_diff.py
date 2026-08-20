#!/usr/bin/env python3
"""Build a compact, faithful representation of an upstream change so an LLM
can actually ingest it, instead of a 60KB-truncated raw diff.

Strategy:
  * Prefer the GitHub API's per-file `patch` (real code, small) -- send fully.
  * For generated data-table files (ip_security_cfaa_block.go,
    ip_blocker_block.go) where the API elides the huge packed blob, DON'T send
    the blob: extract the meaningful header constants via regex from the raw
    file at both revisions and emit "<const> <before> -> <after>".
  * Shape any oversized real-file patch to head+tail with a trim marker.
  * The caller picks the mode:
       compact_commit_diff.py <repo> <sha>            # single commit
       compact_commit_diff.py <repo> <a>...<b>        # compare two refs/tags
       compact_commit_diff.py <repo> --pr <number>    # pull-request diff
"""
import json, re, sys, urllib.request

MEANINGFUL_CONSTS = re.compile(
    r"^\s*const\s+(?P<name>"
    r"(?:cfaaBlockedPrefixCount|cfaaBlockedPrefix6Count|blockerBlockedHostCount|blockerPepper|blockerBlockedHostDataSize)"
    r")\s*=\s*(?P<value>[^\n]+)", re.M
)
DATA_TABLE_FILES = {"ip_security_cfaa_block.go", "ip_blocker_block.go"}
PATCH_BUDGET = 30000

def api_json(url):
    req = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json", "User-Agent": "urnetwork-monitor"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())

def fetch_file(repo, sha, path):
    url = f"https://raw.githubusercontent.com/{repo}/{sha}/{path}"
    with urllib.request.urlopen(url, timeout=60) as r:
        return r.read().decode(errors="replace")

def _const_delta_str(repo, sha_before, sha_after, path):
    try:
        before = fetch_file(repo, sha_before, path)
        after = fetch_file(repo, sha_after, path)
    except Exception as e:
        return f"<could not fetch for const diff: {e}>"
    b = {m.group("name"): m.group("value").strip() for m in MEANINGFUL_CONSTS.finditer(before)}
    a = {m.group("name"): m.group("value").strip() for m in MEANINGFUL_CONSTS.finditer(after)}
    changed = [f"{n} {b.get(n,'<absent>')} -> {a.get(n,'<absent>')}" for n in sorted(set(b)|set(a)) if b.get(n)!=a.get(n)]
    if changed:
        return path + ": " + "; ".join(changed)
    return path + ": <data-only refresh, no meaningful header const changed>"

def _shape(repo, files, sha_a, sha_b):
    """files = list of GitHub file dicts. sha_a/b used for data-table const diff."""
    parts = []
    for f in files:
        fname, fpath = f["filename"], f["filename"].removeprefix("v2026/")
        if fpath in DATA_TABLE_FILES:
            delta = _const_delta_str(repo, sha_a, sha_b, fname) if sha_a and sha_b else "<const diff unavailable>"
            parts.append(f"<data-table {fname}: +{f['additions']} -{f['deletions']}>\n{delta}")
            continue
        patch = f.get("patch")
        if not patch:
            parts.append(f"<no patch in API for {fname}: +{f['additions']} -{f['deletions']}>")
            continue
        if len(patch) > PATCH_BUDGET:
            parts.append(f"<patch for {fname} trimmed to 30KB (was {len(patch)}B)>:\n"
                         + patch[:15000] + "\n...[trimmed]...\n" + patch[-15000:])
        else:
            parts.append(f"# {fname}\n{patch}")
    return "\n\n".join(parts)

def compact_commit_diff(repo, sha):
    data = api_json(f"https://api.github.com/repos/{repo}/commits/{sha}")
    files = data.get("files", [])
    parent = (data.get("parents") or [{}])[0].get("sha", "")
    return _shape(repo, files, parent, sha)

def compact_compare_diff(repo, sha_a, sha_b):
    data = api_json(f"https://api.github.com/repos/{repo}/compare/{sha_a}...{sha_b}")
    return _shape(repo, data.get("files", []), sha_a, sha_b)

def compact_pr_diff(repo, number):
    files = []
    page = 1
    while True:
        page_files = api_json(f"https://api.github.com/repos/{repo}/pulls/{number}/files?per_page=100&page={page}")
        files.extend(page_files)
        if len(page_files) < 100:
            break
        page += 1
    return _shape(repo, files, None, number)

if __name__ == "__main__":
    repo = sys.argv[1] if len(sys.argv) > 1 else "urnetwork/connect"
    if len(sys.argv) >= 4 and sys.argv[2] == "--pr":
        print(compact_pr_diff(repo, sys.argv[3]))
    elif len(sys.argv) >= 4:
        print(compact_compare_diff(repo, sys.argv[2], sys.argv[3]))
    else:
        sha = sys.argv[2] if len(sys.argv) > 2 else "6813e7883300cef040cd5fe0cab6b4ac15bddb6b"
        print(compact_commit_diff(repo, sha))
