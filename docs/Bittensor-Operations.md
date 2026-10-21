# Bittensor Subnet 25: Overview for Operators

> [!TIP]
> **Most operators can stop at the [Quick Start](Bittensor-Quick-Start.md).** You sign one message from your own computer and your nodes keep running as they are. Everything below is background, or for people who want more.

> [!NOTE]
> Checked against upstream server `50851997d`, connect `f1837db6`, sdk `23b3c7dd` and sn `83e6f48c2` (2026-10-21). Upstream is still changing the Bittensor side quickly. If a command prints something different from this page, trust the command.

## The short version

- URnetwork is **Subnet 25 (SN25)** on Bittensor. Your nodes can earn the SN25 token (alpha, branded $UR) on top of normal URnetwork earnings.
- You get paid by signing **one** consent with your Bittensor wallet (your coldkey). It says "pay every node on my URnetwork account to this coldkey". One signature covers all your nodes, now and later.
- Check where you stand with `provider wallet status`. Words you do not know are in the [Glossary](Bittensor-Glossary.md).

## Pool or head: which one am I?

| | Pool tier (almost everyone) | Head tier (optional, advanced) |
|---|---|---|
| What it is | Your nodes earn inside URnetwork's shared pool. | You get your own seat (UID) on the subnet. |
| What you do | Sign the consent once. | A long setup with keys, a fleet file, a burn and per-node binding. See [Head Tier](Bittensor-Head-Tier.md). |
| How you are scored | By the work your nodes do in the pool: usage weighted by quality, with quality kept between 0.75 and 1.0. | By how many different IP ranges (/29 IPv4, /48 IPv6) your nodes served traffic through. |
| How you are paid | Credited to your coldkey every epoch (7 days). A claim moves it to your wallet. | Paid directly to your hotkey every tempo (about 72 minutes). No claim. |
| Where the money waits | In the settlement vault contract (`STSettlementVault` for v1, `STSettlementVault2` for v2). | Nowhere. It is paid by the network. |
| Up-front cost | None. | A registration burn plus gas, and you can lose the seat. |
| Earnings promise | None is promised. A share that rounds to zero pays nothing. | None is promised. The seat can be lost. |

If your head seat is lost, your nodes fall back to the pool. A node earns in only one tier at a time.

## Claims, explained simply

Your pool earnings are credited to your coldkey. A **claim** is a small blockchain transaction that moves the credit into your wallet.

- Anyone can send the claim. The money always goes to your coldkey.
- **Optional.** You can wait until the balance is worth the gas fee.
- **Settlement v2** starts at epoch 3: block 9,348,028, about 2026-10-23 18:10 UTC. Upstream has decided this date, but it still depends on the release checks passing, and the launch is not final yet. In v2, credit **never expires**.
- **Settlement v1** covers the earliest epochs. Its mainnet policy keeps a claim open for 8 epochs (56 days) plus 1 epoch of grace. After that the policy's action is `carry_same_operator`. What that means for your own payout is not documented here, so claim v1 epochs within that window if they matter to you.
- The easiest way may be a claim button in the apps or on ur.io. The SDK the apps use now has a pool claim call. Whether the app you have shows a button is **unconfirmed**.
- Command-line claims exist for people who want automation. They need an EVM key with TAO for gas. See [Claiming by hand](#claiming-by-hand-advanced) and [Claim Daemons](Bittensor-Claim-Daemons.md).

## Signing the consent (reference)

The Quick Start has the steps. In short:

```bash
provider auth                          # on your own computer; paste the code at the prompt
provider wallet sign <coldkey_ss58>    # opens a local page for your wallet extension
provider wallet status                 # confirm
```

Options:

- `provider wallet sign [<coldkey_ss58>] [--port=<port>] [--no-open]`. Leave the address out to choose the account on the page. The page itself is described in [the wallet-consent README](../scripts/wallet-consent/README.md).
- No browser: `provider wallet challenge <coldkey_ss58>` prints the exact text (it signs nothing). Sign it elsewhere, then run `provider wallet set <coldkey_ss58> --message=<text> --signature=<hex>`.
- `provider wallet set <coldkey_ss58> --coldkey_seed_file=<path>` signs from a seed file. Use it only on your own computer, never on a node. The `provider` binary only checks the seed file's ownership and mode. `urnet-tools wallet sign` and `urnet-tools wallet set --coldkey_seed_file` also refuse when the box looks like a fleet member, unless you pass `--i-am-on-my-own-workstation`.
- `--wallet-from-epoch=<e> --wallet-through-epoch=<e>` pick an exact window. A window that would leave epochs unpaid is refused unless you set `URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1`. The window spans at most 65,536 epochs. A newer consent must start at a later epoch than the one it replaces.
- A consent for one node only (a per-provider consent) wins over the network consent for that node, and it is listed per client in `wallet status`.

The old unsigned request (`provide --wallet=...`, `wallet set --legacy-network-wallet`, `POST /sn/wallet`) is not a consent and is not paid. `--legacy-network-wallet` is a local CLI gate: it lets this client send the request, but it cannot make the platform accept it, and a direct `POST /sn/wallet` is not controlled by the flag at all. Without the flag the CLI refuses and the provider keeps providing. Do not use it.

Docker users: you do not change the container command for this. A Docker run that still passes `--wallet=<coldkey>` and `--legacy-network-wallet` can drop both.

## Other pages, only if you need them

- [Hotkey Delegation](Bittensor-Hotkey-Delegation.md): only if you sign in with a hotkey across operators.
- [Multi-Operator](Multi-Operator.md): only when more than one operator is listed. Today there is one.
- [Claim Daemons](Bittensor-Claim-Daemons.md): optional automatic claims.
- [Head Tier](Bittensor-Head-Tier.md) and [Fleet](Bittensor-Fleet.md): the advanced head-tier path.

## Claiming by hand (advanced)

You do not need this if you use the app button or wait. `provider claim` logs in with the token of a client that served traffic, because only such a client has a payout. Pass exactly one of:

| Flag | Credential |
|---|---|
| `--store-client=<key>` | One identity from `~/.urnetwork/.client_jwts.json`. The key is a proxy address or `direct`. |
| `--provider-jwt=<path>` | A client token in a file. |
| `--legacy-coldkey=<coldkey_ss58>` | The network token plus this coldkey. Only for an epoch without a provider artifact. |

To print the claim calldata without sending anything (safe, keeps keys off the node):

```bash
provider claim --store-client=direct --epoch=<epoch>
```

To submit it yourself, add `--rpc=<rpc_url> --key_file=<path>`. Add `--dry-run` to check without sending. `provider snclaim` is a low-level tool that takes the calldata (`provider snclaim --help` lists its commands, including `v2 status`, `v2 claim` and `v2 check`).

## bind-head and unbind-head (old head-tier path)

`provider bind-head` and `provider unbind-head` are the head-tier path from before the `provider fleet` group. They are still in the binary, with their own `--key_file` flow, but **do not use them for new work**. Use `provider fleet` ([Fleet](Bittensor-Fleet.md)). Their grammar is in `provider --help`:

```text
provider bind-head --manifest=<file> --hotkey_seed_file=<file> --valid_from_epoch=<n> --valid_to_epoch=<n> [--client_id=<hex16>] [--client_seed_file=<file>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
provider unbind-head --manifest=<file> --effective_epoch=<n> [--client_id=<hex16>] [--client_seed_file=<file>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
```

## The status dashboard (`sn-status`)

Optional. It is a read-only summary:

```bash
urnet-tools sn-status          # or: urnet-docker sn-status, provider sn-status
urnet-tools sn-status --json
```

Look for the `Wallet consent` line. A green line with your address and "in effect now" is what you want. Example shape, with placeholder values:

```text
  Provider:         urnetwork.service (State: /var/lib/urnetwork)
  Wallet consent:   5... (epochs N through M, in effect now)
  Subnet Epoch:     #<epoch>
  Contract:         0x... (Chain ID: 964)
```

> [!NOTE]
> The "Global Rank" and tier lines on this screen are the URnetwork **bandwidth leaderboard**. They are **not** head-tier eligibility and have nothing to do with the Top 200 seats. Ignore them for Bittensor purposes. Trust the `Wallet consent` line over the `Coldkey (SS58)` line.

## FAQ

### Do I have to do anything?
Run `provider wallet status`. If it shows a network consent in effect, you are done. If not, sign once from your own computer. See the [Quick Start](Bittensor-Quick-Start.md). Nothing else is required, and your nodes keep providing either way.

### I linked my wallet on the website or in the app. Am I paid?
Not necessarily. The older "connect wallet" link is only a login proof, and payouts do not use it. A signed consent is what counts. Confirm with `provider wallet status`. If it says `NOT a consent`, sign once.

### Why did my browser wallet not pop up?
You probably ran the old unsigned path (`provide --wallet=`, `--legacy-network-wallet`). That sends an address without a signature, and without a signature nothing gets paid. Use `provider wallet sign`, which does open your wallet.

### What can the signature do?
It covers every node on your URnetwork account. Your recovery phrase never leaves the wallet extension, which shows exactly what it signs. The signature cannot move your funds. It decides which wallet gets paid.

### Do I need a UID to earn?
No. Pool operators have no UID. Only head fleets hold a seat.

### Do I need a hotkey?
Not unless you do the head tier or sign in with a hotkey.

### Do I need to renew the consent?
No. It lasts 65,536 epochs. Sign again only to change the wallet.

### What happens to claims while a claim daemon is stopped?
Nothing is lost. Signed entries are saved and finished after a restart. See [Claim Daemons](Bittensor-Claim-Daemons.md).

### Is the head tier worth it?
For most operators, no. See [Head Tier](Bittensor-Head-Tier.md) for what it takes and the current blockers.

## Safety rules

1. Never put a token or seed in a command line where `ps` can show it.
2. Keep `~/.urnetwork/jwt` and any key files at mode `0600`.
3. Give each container its own volume so state does not get mixed up.
4. Seed files (coldkey, hotkey, client keys) belong on your own computer, never on a mining node. The tool never creates them for you.

## Third-party licenses

Provider builds statically link the vendored substrate RPC client. `THIRD_PARTY_LICENSES.md` lists every third-party component and its license. Inside `third_party/go-substrate-rpc-client`, the `gethrpc` package is LGPL-3.0 and the rest is Apache-2.0. The full source of the linked version is in this repository at the vendored path.
