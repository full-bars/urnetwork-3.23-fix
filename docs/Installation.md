# Installation Guide

This guide covers the Linux installer, user-level systemd service, post-install commands, and host optimization tools.

## Quick Start

The provider is designed to run as a **non-privileged user service** for maximum security and reliability.

> [!IMPORTANT]
> Recommended: run this command as your normal non-root user. If run as root, the installer will guide you through creating a dedicated service user named `urnet`.

Install:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

Uninstall:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Uninstall_Linux.sh | sh
```

## User-Level Systemd Service

Unlike traditional services that run as root, this build defaults to a **systemd user unit**.

- **Security:** the provider binary does not need root privileges.
- **Isolation:** configuration and JWT tokens are stored in the user's home directory.
- **Linger:** the installer enables `loginctl enable-linger`, so the provider starts automatically on boot and keeps running after logout.
- **Root guard:** if installed as root, the script can create a restricted `urnet` user and add it to the appropriate admin group.

## Post-Install Commands

The installation includes the `urnet-tools` suite for management:

| Command | Description |
| :--- | :--- |
| `urnet-tools status` | Check service health and uptime. |
| `urnet-tools logs` | Stream logs, automatically detecting RAM vs disk logging. |
| `urnet-tools auto on` | Enable Smart Auto. Recommended for most hosts. |
| `urnet-tools optimize` | Full host optimization for kernel, storage, and reliability. Add `-f` to skip prompts. |
| `urnet-tools turbo v4` | Enable Turbo V4 mode. |
| `urnet-tools turbo v8` | Enable Turbo V8 mode. |
| `urnet-tools eco on/off` | Toggle Eco mode. |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools update` | Upgrade to the latest version. |

## System Auditor & Host Optimization

When the provider starts, it logs a **System Auditor** report that checks kernel limits and disk I/O performance:

```text
[audit] Conntrack Max: 262144 (Suboptimal! Target: 2097152)
[audit] Hint: Container detected suboptimal host limits. Run 'urnet-tools optimize' on the HOST to fix.
```

The provider cannot modify host-level kernel settings from inside a container. If you see `Suboptimal!` warnings, run `urnet-tools optimize` on the host machine.

For Docker-only users who do not want the systemd provider service, run the installer on the host to install the tools:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/main/scripts/Provider_Install_Linux.sh | sh
```

Then optimize the host:

```bash
sudo urnet-tools optimize -f
```

The `-f` flag skips interactive prompts. This applies:

- Conntrack max: `262144` -> `2097152`
- Conntrack timeout: `432000s` -> `5400s`
- TCP established timeout: 5 days -> 1 hour
- BBR congestion control and Fair Queuing
- Auto-install of `zram` and `conntrack-tools`
- Boot persistence for kernel modules

After optimization, your Docker container should restart and report:

```text
[audit] Conntrack Max: 2097152 (Optimal!)
```

> [!NOTE]
> If you only run Docker and do not intend to use the systemd provider service, the installer still offers just the tools. Choose `n` when prompted to enable the systemd service.
