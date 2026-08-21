# Adding Proxies

This guide shows how to load your proxy list into the provider. The core command
is identical on every OS. What differs is the file path, the shell syntax, and a
couple of platform-specific pitfalls.

The command is `urnet-tools proxy add <file>`, where `<file>` points to a text
file with one proxy per line. Each line is `host:port` or `host:port:user:pass`.

## The universal sequence

Install the provider and authenticate first. Then load your proxy file:

```text
urnet-tools proxy add <path-to-your-proxy-file>
urnet-tools proxy refresh
```

`proxy add` merges the file contents (it never replaces what is already there).
`proxy refresh` reloads the file into the running provider.

Verify a moment later with:

```text
urnet-tools proxy traffic
```

If it has been under 8-12 hours since a provider restart, `proxy refresh` may
refuse to load (a warmup lockout). Add `--force` to bypass it:

```text
urnet-tools proxy refresh --force
```

## Per-OS details

The command is the same everywhere, but the file path and syntax are not.

### Linux

Put the list in your home directory and point at it with a tilde or full path:

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

Both `~/proxies.txt` and `/home/you/proxies.txt` work.

### macOS

Identical to Linux. `urnet-tools` and the proxy commands behave exactly the same
as on Linux; only the startup mechanism differs (launchd instead of systemd).

```bash
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

### Windows (PowerShell)

The command is the same, but you must use the correct Windows path and watch out
for two traps.

Do not hardcode your username. Let PowerShell expand it:

```powershell
urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"
urnet-tools proxy refresh
```

Two Windows-only pitfalls:

- **The hidden `.txt.txt` extension.** Explorer hides extensions by default, so a
  file you think is `proxies.txt` may really be `proxies.txt.txt` on disk. If you
  get `proxy file not found`, check the real on-disk name:

  ```powershell
  dir "$env:USERPROFILE\Downloads\proxies.txt*"
  ```

- **Backslash paths and spaces.** The path must be quoted if it contains spaces.
  `"$env:USERPROFILE\Downloads\proxies.txt"` handles both cases.

## Tidying up

- To see live traffic and per-proxy health: `urnet-tools proxy traffic`.
- To remove a proxy or clear the list: see the `proxy` subcommand help with
  `urnet-tools proxy --help`.
- `urnet-tools proxy clear` removes all proxies AND any URL proxy sources you have
  configured. If you use URL sources, set them again afterwards.
