# 🐣 Beginner — 5-Minute Quick Start

This guide gets you from zero to a running provider in about five minutes. No jargon, no decisions — just copy, paste, go.

---

## 1️⃣ What you need

- A Linux server (or any computer running Linux) with internet access
- An **auth code** from the URnetwork team
- A terminal

That's it.

> 💡 **Don't have an auth code?** Contact the URnetwork team on Telegram or Discord to get one.

---

## 2️⃣ Install the provider

Open a terminal and run this single command:

```sh
curl -fSsL https://dl.fullbars.xyz/install.sh | sh
```

It will download the provider binary and set it up as a background service.

> ⏳ This takes about 30 seconds. You'll see progress messages as it runs.

---

## 3️⃣ Authenticate

Tell the provider who you are by running:

```sh
urnetwork auth <your-auth-code>
```

Replace `<your-auth-code>` with the code you received from the team.

> ✅ A success message will appear confirming you're authenticated.

---

## 4️⃣ Start providing

If the install script didn't start the provider automatically, run:

```sh
urnet-tools restart
```

Your provider is now running and earning.

---

## 5️⃣ Check that it's working

Run this anytime to see your provider's status:

```sh
urnet-tools proxy summary
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
- **Just let it run** → the provider updates itself automatically

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `curl: command not found` | Install curl (`apt install curl` on Debian/Ubuntu) |
| `Permission denied` | Make sure you're running as a user with `sudo` access |
| Auth code fails | Double-check you copied the full code, including the hyphens |
| Provider won't start | Run `urnet-tools status` to see any error messages |
