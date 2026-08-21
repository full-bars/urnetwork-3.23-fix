#!/usr/bin/env python3
"""Build a compact, faithful representation of an upstream change so an LLM
can actually ingest it, instead of a 60KB-truncated raw diff.

Strategy:
  * Prefer the GitHub API's per-file `patch` (real code, small) -- send fully.
  * For generated data-table files (ip_security_cfaa_block.go,
    ip_blocker_block.go) where the API elides the huge packed blob, DON'T send
    the blob: extract the meaningful header constants via regex from the raw
    file at both revisions and emit "<const> <before> -> <after>".
  * For files the 3.23-fix fork does NOT carry, or whose generated table has
    diverged from upstream, append a fork-aware annotation (FORK LACKS /
    FORK DIVERGED) so the monitor's LLM does not emit a wrong [MUST PORT].
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

# Fork-aware manifest for the 3.23-fix production fork. When an upstream
# change touches one of these, the compact diff emits a hard annotation so the
# monitor LLM cannot guess. Keep this in sync with the fork's actual tree.
#
#   - LACKS: the fork does not carry the file (verify: file + consumer absent).
#     Verdict driver: [NO ACTION] for changes touching only that file.
#   - DIVERGED: the fork carries the file but its generated table is a fork
#     snapshot that may drift from upstream. This is NOT a "never port" rule --
#     the fork periodically syncs the table from upstream (see git history of
#     ip_security_cfaa_block.go). The annotation carries the fork's REAL
#     per-constant counts so the model can compare per constant and classify:
#     a family where the fork lags upstream is a genuine [WATCH] port
#     candidate; a family where the fork already leads is [NO ACTION]. The
#     up/current-count values (below) are a dated snapshot; refresh them when
#     the fork next syncs to avoid stale guidance.
FORK_AWARE = {
    # fork has no ip_blocker_block.go nor its consumer ip_mux_upgrade.go
    "ip_blocker_block.go": "FORK LACKS: ip_blocker_block.go (and its consumer "
                           "ip_mux_upgrade.go) are absent from the 3.23-fix "
                           "fork. Do not mark [MUST PORT] for this file. "
                           "Classify changes to this file as [NO ACTION]; a "
                           "code-path fix here cannot apply to a fork that "
                           "does not carry the file. Judge any other file "
                           "changed by the same upstream commit on its own "
                           "merits.",
    "ip_mux_upgrade.go": "FORK LACKS: ip_mux_upgrade.go is absent from the "
                         "3.23-fix fork (it consumes ip_blocker_block.go, "
                         "which the fork also does not carry). Do not mark "
                         "[MUST PORT] for this file. Classify changes to this "
                         "file as [NO ACTION]; a code-path fix here cannot "
                         "apply to a fork that does not carry it.",
    # fork HAS the file but its generated CFAA table is a fork snapshot
    "ip_security_cfaa_block.go": "FORK DIVERGED: the 3.23-fix fork carries its "
                                  "own CFAA table, a dated snapshot that drifts "
                                  "from upstream (current fork counts: "
                                  "cfaaBlockedPrefixCount=44225, "
                                  "cfaaBlockedPrefix6Count=513). These are NOT "
                                  "the same values as upstream; do not assume a "
                                  "raw port is correct or that the fork leads. "
                                  "Judge PER CONSTANT against the fork counts "
                                  "above: if this refresh grows a family where "
                                  "the fork LAGS (e.g. IPv6 513 < upstream), "
                                  "that is a genuine [WATCH] port candidate; if "
                                  "the fork already LEADS that family (e.g. IPv4 "
                                  "44225 > upstream), classify as [NO ACTION].",
}


def fork_aware_note(path):
    """Return the fork-aware annotation for an upstream file, or '' if none."""
    return FORK_AWARE.get(path, "")


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
        note = fork_aware_note(fpath)
        if fpath in DATA_TABLE_FILES:
            # A fork-LACKS table needs no upstream const fetch (the fork does
            # not carry it; upstream counts are irrelevant -- the annotation is
            # the verdict driver). Absent revisions (PR mode passes None/None)
            # also skip the fetch. Keep the fallback wording generic so a
            # DIVERGED table in PR mode is not mislabelled as LACKS.
            skip_fetch = note.startswith("FORK LACKS")
            if sha_a and sha_b and not skip_fetch:
                delta = _const_delta_str(repo, sha_a, sha_b, fname)
            else:
                delta = "<const diff unavailable (no const delta in this mode)>"
            note_suffix = ("\n" + note) if note else ""
            parts.append(f"<data-table {fname}: +{f['additions']} -{f['deletions']}>"
                         f"\n{delta}{note_suffix}")
            continue
        patch = f.get("patch")
        if not patch:
            # No patch from the API for this file. If it is a fork-aware file,
            # the annotation must survive so the model still gets the verdict
            # guidance (it is authoritative even without patch content).
            note_suffix = ("\n" + note) if note else ""
            parts.append(f"<no patch in API for {fname}: +{f['additions']} "
                         f"-{f['deletions']}>{note_suffix}")
            continue
        if note:
            parts.append(f"# {fname}\n{note}\n{patch}")
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
