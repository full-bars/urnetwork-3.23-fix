# Live status snapshot contract (schema v1)

These fixtures are the wire contract between the provider (`provider/node_snapshot.go`,
served by the control socket `snapshot` command) and `urnet-tools`
(`internal/urnettools`). Both sides have a test that loads them; change a fixture only
together with both structs.

Rules: fields are only ever added, never renamed or removed. Optional fields
(`previous_version`, `idle_hint`, `mem_limit_bytes`, `rss_bytes`, `open_fds`, `fd_limit`)
are omitted when unknown, never zeroed. Readers must ignore unknown fields.

- `state`: `starting` (uptime under 120 s), `degraded`, `idle`, `flowing`. The tool adds `stopped`
  itself when nothing answers.
- `restart.reason`: `update`, `hotswap`, `manual`, `clean`, `unclean`, `first-start`.
- `rate.history_bps`: per-second samples, oldest first, newest last, at most 600.
- Restart marker file (`<state dir>/.restart-reason`): one line, `<reason> <RFC3339 UTC time>`.
  The provider consumes and deletes it at start and ignores one older than 10 minutes.
