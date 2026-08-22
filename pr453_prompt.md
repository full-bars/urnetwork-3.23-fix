You are reviewing PR #453: "feat: in-place container update + real per-command help".

Focus:
1. The new urnet-docker `update <target>` command (cmdDockerUpdate + cobra wiring): correct target flag gating (only --unit/--user/--network/--network-id/--state-dir trigger in-container; else host self-update unchanged)? confirm gate + force/dry-run handled? any regression to the host self-update, its -f/--bogus error propagation, or TestRunDockerSelfUpdateUnknownFlagPropagates?
2. The Cobra per-command help: proxy refactored into a cobra parent with per-subcommand children forwarding to cmdDockerProxy. Does each proxy subcommand still dispatch correctly (args + target flags flow through)? Any cobra arg-parsing regression for `proxy add <file>` / `proxy remove --all`? Is `proxy` bare (no subcommand) behavior preserved (exit 0, prints help)? exec help routing (pre-separator -h -> exec help; help after '--' forwarded)?
3. Help Long/Example text: accurate, STE100 (short sentences, no em dashes, no internal jargon), no fabricated flags.
4. Tests: build/vet/full suite pass locally; cross-builds pass. Any missing test?
5. gitignore: urnet-tools/*.exe added — appropriate?

Report numbered findings with severity + exact file:line. If no real defects, say "NO REAL DEFECTS FOUND".
