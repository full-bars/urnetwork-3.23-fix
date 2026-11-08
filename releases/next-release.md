# Next release

I added extender setup for hosts that need the extender role.

## Extender setup

- Setup checks whether TCP 443 is occupied, whether the provider binary can bind it, and whether the host firewall allows the ports.
- It prints the exact commands and asks before changing anything. Only explicit `--yes` (`-y`) or `--force` approval permits non-interactive changes.
- UDP 53 is optional and user-managed. Setup does not open it.
- FreeBSD extender setup is untested this release. It is print-only. It prints the `sysctl` commands and their undo, and makes no changes. Regular providing on FreeBSD is unaffected.
- On firewalld, setup plans runtime and permanent rules separately. It does not reload firewalld.
- On Linux, setup preserves existing file capabilities when it adds `cap_net_bind_service`.

## Known limitations

- The Docker RAM log follower may replay retained lines after the log is trimmed.
- A hot-swap candidate can race the old provider while it still holds a listening socket. This release does not fix that race.
