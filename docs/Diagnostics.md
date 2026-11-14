# Capture provider diagnostics

Use `urnet-tools diag` to capture evidence from a running provider without restarting it. For setup, see [Installation](/full-bars/urnetwork-3.23-fix/wiki/Installation).

```sh
urnet-tools diag
```

The provider writes a heap profile, goroutine stacks, `summary.txt` and, when available, a 30-second CPU profile under `<state dir>/diag/<UTC timestamp>/`. The request uses the provider's owner-only control socket. Update the provider first if it does not support this command.

The capture needs at least 512 MiB of free space on the state filesystem. Only one capture runs at a time. The provider keeps the five newest bundles and removes an incomplete capture if a hot swap interrupts it.

Treat the bundle as sensitive. Profiles can contain session tokens or other private data. Review it before sharing and use a private channel.

## What you see and what to do when it fails

Run `urnet-tools diag` as the user that runs the provider. The command prints progress and takes about 40 seconds. On success, it prints `Diagnostic bundle:` followed by a path, then the memory use, the goroutine count and the file names. Read `summary.txt` in that directory first.

| Message | What to do |
| :--- | :--- |
| `not allowed to open the control socket` | The socket is owner-only. Run the command as the provider user or with `sudo`. |
| `the control socket ... is not available` | The provider may still be starting. Run `urnet-tools status`, wait a minute and run `diag` again. |
| `did not answer within ...` | A capture may still be running. Look for a new bundle under `<state dir>/diag/`, then try again in a minute. |
| `connection ... closed before it answered` | A hot swap or restart interrupted the capture and left no files. Run `diag` again when `urnet-tools status` shows the provider running. |
| `too old to know diag` | Run `urnet-tools update`, then `urnet-tools hotswap`, then run `diag` again. |
