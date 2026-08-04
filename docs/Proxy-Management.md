# 🔄 Proxy Management & Hot-Reloading

This guide covers how to monitor, update, and prune your proxy list dynamically without restarting the provider.

## 🛑 The Problem with Restarts

> [!WARNING]
> Proxies on the UrNetwork platform take roughly 8–12 hours to fully "warm up" and reach maximum traffic allocation. 
> 
> Previously, if you discovered a single dead proxy in your list, removing it meant restarting the entire provider. That restart wiped the warm state of *every other live proxy* on the machine, causing a temporary dip in earnings and throughput while the backend slowly re-ramped them.

With `v3.23.0-fix.17`, that is no longer the case. 🎉

## ⚡ Proxy Hot-Reload

You can now add and remove proxies from a running instance without a restart. Changes to your proxy list are applied live:
- 🪦 **Dead/removed proxies** are cancelled and their connections drained gracefully.
- 🌱 **New proxies** are started with a staggered delay to prevent a burst of simultaneous authentication attempts at the API.

> [!TIP]
> When a reload adds proxies, the log prints `[proxy] reload: adding N proxies:` followed by the same per-proxy listing used at startup — so you can confirm a `proxy refresh` or `--proxy_url` pickup actually added what you expected, the same way you'd verify a fresh container start.

> [!NOTE]
> **Stable Proxy Slots**
> 
> Proxy slot assignments (`proxy[N]`) are now **stable across reloads.** A proxy's slot number is assigned once and persisted to `proxy.state` in your configuration directory. 
> 
> If you remove a proxy and later re-add it, the provider will recognize it and restore its original slot number, ensuring your logs and traffic reports remain consistent over time.

---

## 📝 Modifying the Proxy List

To update your proxy list:

1. Edit your proxy configuration file (e.g. `proxies.txt` or `proxy.txt`) as you normally would.
2. Run the `refresh` command to apply the changes live.

### 🐧 Binary / Linux Service
```sh
urnet-tools proxy refresh
```

### 🐳 Docker
*(Assuming your container is named `urfix` and you have `urnet-tools` mapped or installed)*
```sh
docker exec -it urfix urnet-tools proxy refresh
```

**What it does:** The command reads your updated configuration, compares it against the live running state, and displays a diff of what will be added and removed. It will then ask for your confirmation before applying the changes to the live provider.

> [!TIP]
> Triggering a reload that results in an **empty proxy list** will exit the provider cleanly. This can be used as a controlled shutdown mechanism without sending a hard `SIGTERM`.

---

## 🧹 Removing Dead Proxies Interactively

If you want to clean up failing proxies without manually editing files, you can use the interactive cleanup tool.

### 🐧 Binary / Linux Service
```sh
urnet-tools proxy remove-dead
```

### 🐳 Docker
```sh
docker exec -it urfix urnet-tools proxy remove-dead
```

> [!IMPORTANT]
> **What it does:** 
> 
> The provider continuously monitors proxy health. This command queries the live health state and groups failing proxies into two categories:
> 1. 💀 **Dead Proxies:** Proxies that have *never* successfully authenticated (likely bad credentials or unreachable IPs).
> 2. 💤 **Inactive/Degraded Proxies:** Proxies that were previously working but have been offline for an extended period.
> 
> The tool will prompt you separately for each category, allowing you to selectively remove dead proxies while keeping inactive ones (in case they are just suffering a temporary network blip), or wipe all failing proxies at once.

---

## 🎯 Removing Proxies by Pattern

Remove every proxy from a given provider or IP range in one command — no list reset, no provider restart. Matching is a case-insensitive substring test against the proxy **host only** (never the port or credentials).

### 🐧 Binary / Linux Service
```sh
urnet-tools proxy remove --match=dc.decodo.com --preview   # list matches, change nothing
urnet-tools proxy remove --match=dc.decodo.com             # prompt, then remove
urnet-tools proxy remove --match=192.3. --yes              # no prompt (IP prefix match)
```

### 🐳 Docker
```sh
docker exec -it urfix urnet-tools proxy remove --match=dc.decodo.com
```

> [!IMPORTANT]
> **What it does:**
>
> 1. Removes matching proxies from **all three stores**: the internal proxy list (`proxy.json`), your proxy file (`--proxy_file`), and the URL source cache.
> 2. Adds the pattern to a persistent **exclude list**, so future URL source refreshes silently skip matching proxies — they can't sneak back in.
> 3. Triggers a **hot reload**: the running provider drops only the removed proxies; everything else keeps serving.

### 🔏 Managing Exclude Patterns

```sh
urnet-tools proxy exclude                        # list active patterns
urnet-tools proxy exclude bad-isp.example        # add a pattern (blocks future URL fetches)
urnet-tools proxy exclude bad-isp.example --remove   # delete a pattern
```

Active patterns also appear in `urnet-tools proxy summary` under **URL Sources**.

---

## 📊 Monitoring Proxy Health and Traffic

To help you decide which proxies to prune, use the built-in telemetry commands:

### 🏥 Proxy Health
View a live report of proxy states (Up, Down, Dead, Degraded) and stream live recovery/loss events.
```sh
urnet-tools proxy health                             # Binary
docker exec -it urfix proxy-health                   # Docker
```

> [!TIP]
> **Understanding "Connecting" and "Dead" Proxies in the First Hour**
>
> When you first deploy proxies or add new ones to your list, they appear as `connecting` in the health report during an initial startup period. This is **not** a sign they're broken—it's how the system stages massive deployments.
>
> **Why:** The backend cannot handle thousands of proxies authenticating all at once, so the provider staggers them with a 100ms delay between each proxy. This means:
> - A 1,000-proxy deployment takes ~100 seconds to fully initialize
> - A 5,000-proxy deployment takes ~8 minutes
> - A 10,000-proxy deployment takes ~17 minutes
>
> During this time, proxies show as `connecting` simply because they haven't connected yet—not because they're broken.
>
> **When to investigate:** A proxy that has never connected stays `connecting` for one full hourly pulse cycle (~65 minutes); only after that does it fall back to `dead`. If a proxy is *still* `dead` after that window, it's a real problem (bad credentials, unreachable address). But in the first hour, `connecting` labels are normal and expected.
>
> **Pro tip:** Once a proxy successfully connects even once, it's permanently marked as "ever connected." If it drops later, it shows as `degraded` (not `dead`), giving you a clear signal about which proxies worked before vs. which never worked at all.

### 📈 Proxy Traffic
View a sorted report of cumulative bandwidth per proxy, broken down by billable vs. total traffic, along with the number of active NAT sessions currently multiplexed through each proxy.
```sh
urnet-tools proxy traffic                            # Binary
docker exec urfix cat /root/.urnetwork/proxy_traffic.state  # Docker
```
