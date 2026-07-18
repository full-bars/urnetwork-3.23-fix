# 🐣 Beginner — 5-Minute Quick Start

This guide gets you from zero to a running provider in about five minutes. No jargon, no decisions — just copy, paste, go.

---

## 1️⃣ What you need

- A computer or server running **Linux, macOS, or Windows** with internet access
- An **auth code** from the URnetwork team
- A terminal (macOS/Linux) or PowerShell (Windows)

That's it.

> 💡 **Don't have an auth code?** Contact the URnetwork team on Telegram or Discord to get one.

---

## 2️⃣ Install the provider

**Linux:**
```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

**macOS:**
```sh
curl -fSsL https://dl.fullbars.xyz/install-mac.sh | sh
```

**Windows (PowerShell, no admin required):**
```powershell
powershell -c "irm https://dl.fullbars.xyz/install-win.ps1 | iex"
```

It will download the provider binary and set it up as a background service (systemd on Linux, launchd on macOS, a Startup entry on Windows).

> ⏳ This takes about 30 seconds. You'll see progress messages as it runs.

---

## 3️⃣ Authenticate

Tell the provider who you are:

**Linux/macOS:**
```sh
urnetwork auth <your-auth-code>
```

**Windows:**
```powershell
urnetwork auth <your-auth-code>
```

Replace `<your-auth-code>` with the code you received from the team.

> ✅ A success message will appear confirming you're authenticated.

---

## 4️⃣ Start providing

If the install script didn't start the provider automatically:

**Linux/macOS:**
```sh
urnet-tools restart
```

**Windows:**
```powershell
urnet-tools.ps1 start
```

Your provider is now running and earning.

---

## 5️⃣ Check that it's working

Run this anytime to see your provider's status:

**Linux/macOS:**
```sh
urnet-tools proxy summary
```

**Windows:**
```powershell
urnet-tools.ps1 proxy summary
```

You should see something like:
```
Proxies up:  N
Clients:     N
Earning:     yes
```

---

## ❓ What now?

- **Add your own proxy list** → see the [Intermediate Guide](intermediate.md)
- **Tune for performance** → see the [Advanced Guide](advanced.md)
- **Just let it run** → auto-update is on by default on Linux (systemd timer; `urnet-tools auto-update` to check or change the schedule) and Windows (`urnet-tools.ps1 auto-update-enable`/`-disable`). On macOS there's no auto-update yet — run `urnet-tools update` yourself when a new version ships.

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `curl: command not found` | Install curl (`apt install curl` on Debian/Ubuntu; macOS ships curl by default) |
| `Permission denied` | Make sure you're running as a user with `sudo` access (Linux) |
| Auth code fails | Double-check you copied the full code, including the hyphens |
| Provider won't start | Run `urnet-tools status` (Windows: `urnet-tools.ps1 status`) to see any error messages |
