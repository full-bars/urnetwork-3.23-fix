# Bittensor Quick Start: Do I Need to Do Anything?

This page is for regular operators. You run URnetwork nodes and you may have heard about Bittensor Subnet 25 (SN25). Start here. Almost everyone can stop after one step.

## The 30-second answer

- Your nodes keep running exactly as they are. **Do not change anything on your nodes.**
- To get paid on SN25 you sign **one message** with your Bittensor wallet (your "coldkey"). You do it **once**, from **your own computer**. Never from a node.
- That one signature covers all your nodes, now and later. You do not renew it.
- Everything else in the Bittensor docs (hotkeys, claim daemons, fleets, multi-operator) is optional or advanced. Skipping it costs you nothing.

## Step 1: Check where you stand

Run this on any node (it only reads, it changes nothing):

```bash
provider wallet status
```

On a systemd install you can also run `urnet-tools wallet status`. For Docker, run it inside the container, for example `docker exec <container> provider wallet status`. If that says you are not logged in, see the note under "How to confirm it works".

What the answer means:

| You see | What it means | What to do |
|---|---|---|
| `network consent for the network: coldkey 5..., epochs N through M ... in effect now` (or `starts in 1 epoch(s)`) | You are set up. | Nothing. You are done. |
| `login-proof wallet 5... for the network (NOT a consent ...)` and `no consent is stored` | You linked a wallet on the website or app the old way. It only proves who you are. Payouts do not use it. | Do the signing steps below. You already have a wallet, so skip making one. |
| `no wallet is set for this network` | Nothing is stored. | Do the signing steps below. |

## Step 2: The one-time signing steps

Do these on **your own computer** (the one with your wallet's browser extension), not on a node.

1. **Have a wallet.** If you do not have one, install a Polkadot-style browser wallet extension (Talisman, SubWallet, Polkadot{.js} or Nova) and create an account. Write the recovery phrase on paper and keep it offline. The account address starts with `5`. This address is your coldkey.
2. **Get the `provider` program** on that computer and log it in to your URnetwork account with an auth code from the website:
   ```bash
   provider auth
   ```
   Paste the code when prompted rather than passing it as an argument: an argument shows up in `ps` and in your shell history. This only saves your login. It does not start a node.
3. **Sign the consent:**
   ```bash
   provider wallet sign <your_coldkey_address>
   ```
   A page opens on your own computer only. Your wallet extension pops up and shows the message. It starts with `Approve URnetwork network wallet mapping`. Approve it. The page counts down 15 minutes. Your recovery phrase never leaves the extension. You can leave out the address and pick the account on the page. Add `--no-open` if you want the address printed instead of the browser opening.
4. **Look for the success line:** `subnet wallet consent for 5... stored: epochs N through M ...`, then "You are done. Nothing changes on your provider machines."

Good to know:

- The consent starts at the **next** weekly payout period (an "epoch"), never the current one. Epochs before your first consent are not paid.
- It lasts 65,536 epochs. You never renew it. Sign again only if you want to change the wallet.
- No browser on that computer? Run `provider wallet challenge <coldkey>` to print the exact message (it signs nothing). Sign it somewhere else (for example btcli or a hardware wallet). Then send it within about 5 minutes with `provider wallet set <coldkey> --message='<printed text>' --signature=0x<128 hex characters>`.

## One-line path by install type

The step is the same for everyone. Nothing changes on the nodes.

| You run your nodes with | What to do |
|---|---|
| **Docker** | Nothing in the containers. On your own computer: `provider auth` (paste the code at the prompt), then `provider wallet sign <coldkey>`. |
| **Linux install (urnet-tools)** | Nothing on the node. On your own desktop: `provider auth` (paste the code at the prompt), then `provider wallet sign <coldkey>`. On a node, `urnet-tools wallet sign` refuses on purpose. |
| **Windows** | On the PC with your wallet extension: `provider.exe auth` (paste the code at the prompt), then `provider.exe wallet sign <coldkey>`. This has not been tested end to end on Windows yet, so check with `wallet status` afterwards. |
| **The URnetwork app or ur.io** | App consent signing is unconfirmed. A plain "connect wallet" is **not enough**. If you use the app, run `provider wallet status` afterwards and do not assume the consent exists. |

## How to confirm it works

1. Run `provider wallet status` again. A good result shows `network consent ... in effect now` (or `starts in 1 epoch(s)`). The `receipt` line appears only when the platform returns a mapping hash, so a missing receipt is not a failure.
2. Your nodes just need to be healthy and carrying traffic. Nothing Bittensor-specific runs on a node.
3. Optional dashboard: `urnet-tools sn-status` (Docker: `urnet-docker sn-status`). Look for the green `Wallet consent` line. The "Global Rank" lines there are the URnetwork bandwidth leaderboard. They do **not** say whether you qualify for anything on Bittensor.

If `wallet status` says you are not logged in, the node's saved login (`~/.urnetwork/jwt`) is not where the command looked. For Docker, check which user the container runs as. This is not confirmed for every setup.

## Commands you do NOT need

| Command | Why you can skip it |
|---|---|
| `provide --wallet=<coldkey>` and Docker `--legacy-network-wallet` | Sends the old unsigned request. It is not a consent and payouts do not use it. Remove it from your setup. |
| `provider wallet set <coldkey> --legacy-network-wallet` | Same thing. Unsigned, not paid. |
| `provider wallet hotkey ...` | Only for people who sign in with a hotkey across several operators. Today there is one operator. |
| `provide --all-operators`, `provider operators` | Multi-operator mode. Today there is one operator. |
| `provider fleet ...`, `bind-head`, `unbind-head` | Head tier only. See [the head tier page](Bittensor-Head-Tier.md). |
| `provider claim-daemon`, `claim-daemon-v2`, `provider snclaim` | Optional claim automation and a low-level tool. |
| `provider claim` | Optional manual claim. |
| `sn-status` | Optional dashboard. |

## When you DO need more

- **You want the head tier** (your own seat on the subnet, paid directly). It is advanced, costs money up front, and only makes sense if you control many different internet address ranges. Most operators should not do it. Read [Bittensor Head Tier](Bittensor-Head-Tier.md) first. Keep your consent signed too: nodes you do not bind keep earning in the pool.
- **You sign in with a hotkey** (`--auto-register` with a hotkey seed file). That makes a **new** URnetwork account owned by the hotkey. Your website consent does not cover it. See [Hotkey Delegation](Bittensor-Hotkey-Delegation.md).
- **You want automatic claims.** See [Claim Daemons](Bittensor-Claim-Daemons.md). Most people do not need this.

For the full picture and a FAQ, see [Bittensor Operations](Bittensor-Operations.md). For plain meanings of the words, see the [Glossary](Bittensor-Glossary.md).
