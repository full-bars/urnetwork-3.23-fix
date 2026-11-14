# Blocklist Sync

This page is for maintainers. It explains how the built-in blocklist tables stay current and what to do when the sync stops.

## What the sync does

The provider ships two generated tables. `ip_security_cfaa_block.go` holds the CFAA block list, and `ip_blocker_block.go` holds the IP blocker list. Both are produced upstream. The `CFAA Blocklist Sync` workflow copies them into this repository.

The workflow runs twice a day, at 04:00 and 16:00 UTC. You can also start it by hand from the Actions tab.

| Step | What happens |
| :--- | :--- |
| Fetch | It reads both tables from the upstream main branch and counts the IPv4 and IPv6 prefixes. |
| Skip | If both tables are identical to ours, the run ends. |
| Format check | It runs our decoder and generator tests against the upstream tables. See below. |
| Branch and PR | It commits the tables on `chore/cfaa-blocklist-sync` and opens a pull request with the `security` label. If a pull request from that branch is already open, it reuses it. |
| Notice | It posts the result to the maintainers' chat channel. |

## The format check

Upstream can change the table format and the decoder in the same window. If we copied only the tables, our decoder could not read them. The format check prevents that.

`.github/scripts/cfaa_format_check.sh <upstream-sha>` writes the upstream tables into the working tree, runs the tests, and then restores both table files from the index. Run it only in a clean, disposable checkout, because it discards unstaged edits to those two files. The tests are the ones that match this pattern:

```sh
go test -count=1 -run 'Blocker|Cfaa|CFAA|Packed|Generated|Provenance|Telegram|Reflector|Security' . ./blocker/...
```

If the tests pass, the sync goes on. If they fail, the sync stops before it opens a pull request. The script writes the failed tests and the recent upstream commits that touched the format code to `/tmp/cfaa_sync_info.json`, and the notice names them.

## When the sync is blocked

A blocked notice means one of two things.

- **Upstream changed the format.** Our decoder in `ip_blocker.go` and our generator in `blocker/` must change first. The sync cannot do that. Port the decoder change, then run the sync again.
- **Upstream shipped a table that its own tests reject.** For example, a table can list an address that is also a published Telegram call reflector. The Telegram tests fail on it. Wait for upstream to fix the table, or report it to them. Do not copy the table by hand.

While the sync is blocked, the tables stay at the last refresh that passed. The provider keeps working. It only misses the newest entries.

## Reviewing a sync pull request

A sync pull request changes generated data only. A pull request opened by the workflow does not start the CI checks. Before you merge it, review the table changes and the workflow result, and run the test command above on its branch. If the tests pass, merge it.

## Related files

| File | Purpose |
| :--- | :--- |
| `.github/workflows/cfaa-blocklist-sync.yml` | The workflow. |
| `.github/scripts/cfaa_format_check.sh` | The format check. |
| `.github/scripts/cfaa_sync.sh` | Writes the tables and makes the commit. |
| `.github/scripts/create_pr.sh` | Opens or reuses the pull request. |
| `.github/scripts/write_sync_info.sh` | Records the prefix counts and the upstream commit. |
| `.github/scripts/cfaa_sync_notify.py` | Posts the notice. |
