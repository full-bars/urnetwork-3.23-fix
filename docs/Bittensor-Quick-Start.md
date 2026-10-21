# Bittensor Quick Start: Do I Need to Do Anything?

This page is for regular operators. You run URnetwork nodes and you may have heard about Bittensor Subnet 25 (SN25). Start here. Almost everyone can stop after one step.

> [!NOTE]
> Upstream is still changing the Bittensor side quickly. This page was checked against upstream server `50851997d`, connect `f1837db6`, sdk `23b3c7dd` and sn `83e6f48c2` (2026-10-21). If a command prints something different from what is shown here, trust the command.

## The 30-second answer

- Your nodes keep running exactly as they are. **Do not change anything on your nodes.**
- To get paid on SN25 you sign **one message** with your Bittensor wallet (your "coldkey"). You do it **once**, from **your own computer**. Never from a node.
- That one signature covers every node on your URnetwork account, including nodes you add later. You do not renew it.
- Everything else in the Bittensor docs (hotkeys, claim daemons, fleets, multi-operator) is optional or advanced. Skipping it costs you nothing.

> [!CAUTION]
> Your recovery phrase (seed) never goes on a node, into a command line, into a chat or onto a website. No step on this page asks for it. Signing happens inside your wallet extension, and the phrase never leaves it.

## Step 1: Check where you stand

Run this on any node. It only reads and changes nothing:

```bash
provider wallet status
```

On a systemd install you can also run `urnet-tools wallet status`. For Docker, run it inside the container, for example `docker exec <container> provider wallet status`.

What the answer means:

| You see | What it means | What to do |
|---|---|---|
| `network consent for the network: coldkey 5..., epochs N through M (generation G), in effect now` | You are set up and being paid to that coldkey. | Nothing. You are done. |
| The same line ending in `starts in 1 epoch(s)` | You are set up. Payouts to this coldkey start at the next epoch. | Nothing. Wait for the epoch to turn. |
| The same line ending in `ended` | The consent's epoch range is over. This only happens if a shorter range was chosen on purpose. | Sign again (Step 2). |
| `provider consent for client <id>: ...` | A consent for one single node. For that node it wins over the network consent. | Nothing, unless you meant to pay that node to the network coldkey. See [Changing your wallet](#changing-your-wallet). |
| `login-proof wallet 5... for the network (NOT a consent: release settlement does not pay it)` followed by `no consent is stored` | You linked a wallet on the website or in the app the old way. It only proves who you are. Payouts do not use it. | Do Step 2. You already have a wallet, so skip making one. |
| `no wallet is set for this network` | Nothing is stored. | Do Step 2. |
| An error about not being logged in | The command could not find the node's saved login. | See [Troubleshooting](#troubleshooting). |

## Step 2: Sign the consent once

Do these on **your own computer** (the one with your wallet's browser extension), not on a node.

1. **Have a wallet.** If you do not have one, install a Polkadot-style browser wallet extension (Talisman, SubWallet, Polkadot{.js} or Nova) and create an account. Write the recovery phrase on paper and keep it offline. The account address starts with `5`. This address is your coldkey.
2. **Get the `provider` program** on that computer. The installers in the [README](../README.md#install) work, or download the binary for your system from the releases page. You do not need to run a node on this computer.
3. **Log in** to your URnetwork account with an auth code from the website:
   ```bash
   provider auth
   ```
   Paste the code when prompted rather than passing it as an argument: an argument shows up in `ps` and in your shell history. This only saves your login. It does not start a node.
4. **Sign:**
   ```bash
   provider wallet sign <your_coldkey_address>
   ```
   A page opens at `http://127.0.0.1:8765/` (another free port if that one is taken), so only your own computer can reach it. Unlock your extension, click **1. Connect wallet** and approve, then click **2. Sign** and approve. The extension shows the message. Its first line is `Approve URnetwork network wallet mapping`. You can leave out the address and pick the account on the page. Add `--no-open` to print the page address instead of opening the browser. More detail is in [the wallet-consent README](../scripts/wallet-consent/README.md).
5. **Look for the success line:**
   ```text
   subnet wallet consent for 5... stored: epochs N through M, generation G, receipt ...
   ```
   It is followed by "What happens next" and "You are done. Nothing changes on your provider machines".

Good to know:

- The consent starts at a **later** weekly payout period (an "epoch"), never the current one. Epochs before your first consent are not paid to it.
- It covers 65,536 epochs (more than a thousand years). In practice you never renew it.
- Leave the epoch range at its default. You can pick one with `--wallet-from-epoch` and `--wallet-through-epoch`, but a mistaken range cannot be undone, so a range that would leave epochs unpaid is refused unless you set `URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1`.
- **No browser on that computer?** Run `provider wallet challenge <coldkey>` to print the exact message. It signs nothing. Sign the message somewhere else (for example btcli or a hardware wallet), then submit it within 5 minutes, because the challenge expires 300 seconds after it is issued:
  ```bash
  provider wallet set <coldkey> --message='<printed text>' --signature=0x<128 hex characters>
  ```
- **`urnet-tools wallet sign` refuses on a node on purpose.** If your desktop also runs a provider and the tool mistakes it for a fleet node, and you are sure it is your own computer, add `--i-am-on-my-own-workstation`.

## By install type

The step is the same for everyone. Nothing changes on the nodes.

| You run your nodes with | What to do |
|---|---|
| **Docker** | Nothing in the containers. On your own computer: `provider auth`, then `provider wallet sign <coldkey>`. |
| **Linux install (urnet-tools)** | Nothing on the node. On your own desktop: `provider auth`, then `provider wallet sign <coldkey>`. |
| **Windows** | On the PC with your wallet extension: `provider.exe auth`, then `provider.exe wallet sign <coldkey>`. This has not been tested end to end on Windows yet, so check with `wallet status` afterwards. |
| **The URnetwork app or ur.io** | The SDK the apps are built on can now request and submit a network consent (sdk `bf4076ff`). Whether the app version you have actually offers it is **not confirmed**. A plain "connect wallet" is **not enough**: it only makes a login-proof wallet. After using the app, run `provider wallet status` and do not assume a consent exists. |

## How to confirm it works

1. Run `provider wallet status` again and match the result to the table in Step 1. The `receipt` line appears only when the platform returns a mapping hash, so a missing receipt is not a failure.
2. Your nodes just need to be healthy and carrying traffic. Nothing Bittensor-specific runs on a node.
3. Optional dashboard: `urnet-tools sn-status` (Docker: `urnet-docker sn-status`). Look for the green `Wallet consent` line. The "Global Rank" lines there are the URnetwork bandwidth leaderboard. They do **not** say whether you qualify for anything on Bittensor.

## Changing your wallet

- Sign again with the new coldkey, using the same Step 2. This makes a new generation of the consent.
- A new consent must start at a **later** epoch than the one it replaces. Epochs already covered keep paying the old coldkey. Closed epochs are never rewritten.
- There is no unsigned "remove wallet" button. A consent ends only when a new signed one replaces it, or at its last epoch. This means nobody, including a compromised server, can redirect your earnings without a signature from the new coldkey.
- A node with its own per-provider consent keeps paying to that consent until it is replaced or ends. Then the node falls back to the network consent from the next epoch.

## Troubleshooting

| Problem | What to check |
|---|---|
| `wallet status` says you are not logged in | The node's saved login lives in `~/.urnetwork/jwt` (or under `URNETWORK_STATE_DIR` if set). The command must run as the same user that owns it. For Docker, check which user the container runs as. This is not confirmed for every setup. |
| The message expired before you signed | Each message is valid for 5 minutes. Click **New message** on the page to get a fresh one. |
| The signing page closed | The page closes after 15 minutes. Run `provider wallet sign` again. Nothing was stored. |
| `signature_mismatch` | The signature came from a different account than the coldkey you named. Pick the same account in the extension, or leave out the address and choose it on the page. |
| Refused because the account is not the network owner | Network consent needs the owner's login (the auth code from the website), not a single node's client login. Run `provider auth` on your own computer. |

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

- **You want the head tier** (your own seat on the subnet, paid directly). It is advanced, costs TAO up front, and only makes sense if you control many different public IP ranges. Most operators should not do it. Read [Bittensor Head Tier](Bittensor-Head-Tier.md) first. Keep your consent signed too: nodes you do not bind keep earning in the pool.
- **You sign in with a hotkey** (`--auto-register` with a hotkey seed file). That makes a **new** URnetwork account owned by the hotkey. Your website consent does not cover it. See [Hotkey Delegation](Bittensor-Hotkey-Delegation.md).
- **You want automatic claims.** See [Claim Daemons](Bittensor-Claim-Daemons.md). Most people do not need this.

For the full picture and a FAQ, see [Bittensor Operations](Bittensor-Operations.md). For plain meanings of the words, see the [Glossary](Bittensor-Glossary.md).
