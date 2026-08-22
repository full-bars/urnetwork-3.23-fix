You are reviewing PR: chore(go): bump toolchain to Go 1.27.0 (go.mod 1.26.4 -> 1.27.0, CI pins, both Dockerfiles, docs).

Focus:
1. go.mod `go 1.27.0` directive: correct? any missing build files still referencing 1.26 (Dockerfiles, docker-compose, Makefile, go.work, scripts, .github)?
2. CI go-version pins: all workflows floated to 1.27? any missed workflow or hard-coded 1.26?
3. Docs sweep: README, docs/*.md, wiki .md files, CHANGELOG, FORK_CHANGES - any LIVE reference to Go 1.26/1.26.4 left (ignore HISTORICAL changelog/release entries that record past 1.26 releases - those should stay)?
4. Changelog/FORK_CHANGES entries: accurate, correct PR numbers, STE100 style (short sentences, no em dashes)?
5. Any correctness risk in floating go-version pins (vs pinning patch).

Report findings as numbered list with severity (CRITICAL/MEDIUM/LOW) and exact file:line. If no real defects, say "NO REAL DEFECTS FOUND".
