# v3.23.0-fix.33.0 — Native Alpine Linux (OpenRC) support

## 📦 Quick Start / Install

**🐧 Linux (including Alpine):**
```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

**🍎 macOS:**
```sh
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```

**🪟 Windows (PowerShell):**
```powershell
irm https://dl.fullbars.xyz/install-win.ps1 | iex
```

**🐳 Docker:**
```sh
docker pull ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.33.0
```

**🔄 Updating an existing install:**
```sh
urnet-tools update
```

---

## 🗂️ What's New

**Run the provider as a real service on Alpine Linux.** You can now install on Alpine, and on other hosts that use OpenRC, with the same one-line command every other Linux uses. The installer notices OpenRC on its own and sets the provider up as a system service that starts at boot with nobody logged in, runs as its own unprivileged user, and is restarted automatically by `supervise-daemon` if it crashes. Logs live under the system log directory, and `urnet-tools logs` follows them. There is no lingering to enable, because OpenRC does not need it.

**Manage it with the commands you already know.** On OpenRC, `urnet-tools start`, `stop`, `restart`, `auto-start`, `auto-update` and `logs` work through `rc-service` and `rc-update`, and `auto-update` uses the busybox `crond` that Alpine ships instead of a systemd timer. If another provider is running beside the service, `stop` and `restart` ask you to name the one you mean with `--user`, `--unit` or `--state-dir`, rather than stopping one and reporting success while the other keeps running.

**Authenticate during the install.** When you run the installer from a terminal — the one-line `curl ... | sh` form included — it can ask for your auth code and run the sign-in as the service user, so the login lands where the service will look for it. If you skip the prompt, if the sign-in does not complete, or if there is no terminal to ask on, the installer prints the single command to run later.

**A direct-only node now uses H3 on its own.** A node with no proxy source configured serves traffic from its own address and has a single identity, so the H3 transport and its datagram options are switched on automatically once the node settles. Nothing changes for nodes with proxies, and `urnet-tools set h3 off` still wins if you want it off.

**The `[profit]` log line tells you what kind of node you have.** It now carries `mode=direct`, `mode=proxies` or `mode=mixed`, and `proxies_up` counts real proxies only. A direct-only node used to read as a healthy node with one proxy up. It now reads as `mode=direct` with `proxies_up=0`.

**Installs work on minimal systems.** The installer no longer gives up when `curl` and `wget` are both missing, it installs one. It works with the stripped-down `wget` in busybox. And when neither `jq` nor `python3` is present, it installs a JSON parser instead of stopping.

---

## ⚠️ Things to Know on Alpine

**Updating needs root.** On OpenRC, restarting a service is a root action, in the same way `systemctl restart` is for a system service under systemd. Run `urnet-tools update` from a root shell. The provider's own files are owned by root on purpose, so the account that runs the provider cannot replace the program that root runs on a schedule.

**There is no zero-downtime hotswap on OpenRC.** `supervise-daemon` has no way to hand a running provider over to a new process, so `urnet-tools update` stops the provider and starts it again. There is a brief gap while it restarts, and the command tells you why it did not hotswap. Hosts running systemd are unchanged and keep hotswap.

---

## ⚠️ Breaking Changes

**(None. Drop-in upgrade.)**

---

## What's Changed
<!-- GitHub will automatically append the generated list of merged PRs and commits here. -->
