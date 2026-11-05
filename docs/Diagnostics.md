# Capture provider diagnostics

Use `urnet-tools diag` to capture evidence from a running provider without restarting it. For setup, see [Installation](/full-bars/urnetwork-3.23-fix/wiki/Installation).

```sh
urnet-tools diag
```

The provider writes a heap profile, goroutine stacks, `summary.txt` and, when available, a 30-second CPU profile under `<state dir>/diag/<UTC timestamp>/`. The request uses the provider's owner-only control socket. Update the provider first if it does not support this command.

The capture needs at least 512 MiB of free space on the state filesystem. Only one capture runs at a time. The provider keeps the five newest bundles and removes an incomplete capture if a hot swap interrupts it.

Treat the bundle as sensitive. Profiles can contain session tokens or other private data. Review it before sharing and use a private channel.
