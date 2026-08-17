# 🐣 Beginner: 5-Minute Quick Start

> **Navigation:** [Guides Index](README.md) · **🐣 Beginner** · [🧭 Intermediate](intermediate.md) · [🚀 Advanced](advanced.md)

This guide gets you from zero to a running provider in about five minutes. No jargon, no decisions: just copy, paste, go.

---

## 1️⃣ What you need

- A computer or server running **Linux, macOS, or Windows** with internet access
- An **auth code** (generate one from the [web dashboard](https://app.ur.network), the [ur.io site](https://ur.io/), or the URnetwork mobile app)
- A terminal (macOS/Linux) or PowerShell (Windows)

That's it.

> [!TIP]
> **Need an auth code?** Generate one at [app.ur.network](https://app.ur.network), the [ur.io](https://ur.io/) landing page, or from the URnetwork mobile app. When generating via the web dashboard you can set expiration and usage limits; the mobile app and ur.io default to 5-minute expiration and single use. The auth code is a one-time token exchanged for a JWT — it's safe to type or paste in the terminal (unlike the JWT itself, which is stored on disk after authentication).

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

> [!NOTE]
> Download time varies by connection speed. You'll see progress messages as it runs.

---

## 3️⃣ Authenticate

Tell the provider who you are:

```sh
urnetwork auth
```

The command will prompt you to paste your auth code. Paste it and press Enter. The code won't appear on screen as you type/paste.

You can also pass the auth code directly:

```sh
urnetwork auth <your-auth-code>
```

> [!NOTE]
> If you already have a JWT on file, you'll be asked whether to overwrite it — type `y` and press Enter.

> ✅ A success message will appear confirming you're authenticated.

---

## 4️⃣ Start providing

If the install script didn't start the provider automatically:

```sh
urnet-tools start
```

Your provider is now running and earning.

---

## 5️⃣ Check that it's working

Watch the logs for a client ID to appear — one per proxy deployed (at minimum the VPS IP itself gets one):

```sh
urnet-tools logs
```

When you see a client ID logged, the provider is connected and earning.

---

## ❓ What now?

- **Add your own proxy list** → see the [Intermediate Guide](intermediate.md)
- **Tune for performance** → see the [Advanced Guide](advanced.md)
- **Just let it run** → auto-update is on by default on Linux (systemd timer; `urnet-tools auto-update` to check or change the schedule) and Windows (`urnet-tools auto-update-enable`/`auto-update-disable`). On macOS there's no auto-update yet — run `urnet-tools update` yourself when a new version ships.

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `curl: command not found` | Install curl (`apt install curl` on Debian/Ubuntu; macOS ships curl by default) |
| Auth code fails | The code may have expired (most auth codes default to 5-minute expiration). Generate a new one from [app.ur.network](https://app.ur.network), [ur.io](https://ur.io/), or the mobile app |
| Provider won't start | Run `urnet-tools status` to see any error messages |

---

> Next step: [🧭 Intermediate Guide: Custom Setup & Proxies →](intermediate.md)
