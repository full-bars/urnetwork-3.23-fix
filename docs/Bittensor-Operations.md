# Bittensor Subnet 25 Operations & Commands Guide

This guide details operating procedures, command references, and telemetry monitoring for URNetwork Connect nodes participating in Bittensor Subnet 25. The heavier command families have focused pages: [Bittensor hotkey delegation](Bittensor-Hotkey-Delegation.md), [Bittensor claim daemons](Bittensor-Claim-Daemons.md), [Bittensor fleet](Bittensor-Fleet.md) and [Multi-operator providers](Multi-Operator.md).

---

## 1. Subnet 25 Architecture & Earning Mechanics

URNetwork Connect providers operate on a dual-incentive model:
1. **Network Provider Traffic**: Direct bandwidth earnings based on billable byte delivery and client session routing.
2. **Bittensor Subnet 25 Incentive Pool**: On-chain emissions distributed proportionally to verified miners based on Merkle-proven bandwidth scores across finalized payout epochs.

### Identity & Wallet Model
- **Network Account Token (`jwt`)**: The provider authenticates to the URNetwork control plane using a signed JWT stored at `~/.urnetwork/jwt` (or container volume `/home/urnet/.urnetwork/jwt`).
- **Claim Coldkey (`ss58`)**: Bittensor coldkey address (prefix `42`, e.g. `5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5`) registered with the platform to authorize and receive pool emissions.
- **Mirror Account**: EVM smart contract accounts mirrored on Subtensor via `evm:<H160_address>` hash derivation for non-custodial claims.

### Two Distinct Miner Tiers: Pool Providers vs. Top 200 Head Fleets

Subnet 25 operates a two-channel architecture under a hard 256-UID metagraph ceiling (`urfoundation/sn` WHITEPAPER §8.4, §11.4; `protocol/policy.go`):
`256 >= (Top-Level Head Miners ~200) + (NO-Pool UIDs) + (Validator UIDs)`

The 41% miner emission is divided into two distinct processes:

| Dimension | Tail Providers (Pool Process) | Top ~200 Fleets (Head Miner Process) |
|---|---|---|
| **On-Chain Identity** | **No UID.** Operates as a `client_id` inside an operator's pool. | **Holds its own Subnet 25 Miner UID** (claimed via `burned_register`). |
| **Scoring Basis** | Demand-coupled: `deposit × quality` across the operator's pool. | **Pure measured routable-IP breadth** (`score(u) = Σ 1/claim(h)` across verified /29 IPv4 & /48 IPv6 subnets). |
| **Emission & Payout** | **Custodied in EVM Contract** (`STSubnet.sol`). Requires Merkle inclusion proofs. | **Native Bittensor Protocol Emission** via Yuma Consensus credited as α stake directly to the coldkey. |
| **Payout Timing** | Finalized **once per epoch (~7 days / 50,400 blocks)**. | Deposited **every block (~12 seconds)**. |
| **Gas & Claiming** | **Manual Claim Required:** Operators submit an on-chain `claimMiner` transaction and pay gas. | **Zero Gas, Zero Claims:** Automated native emission. No EVM contract claims, no Merkle trees. |
| **Intermediary** | Operator commits the Merkle payout root (auditable on-chain). | **Trust-minimized:** Direct consensus-to-coldkey. No operator in the payout path. |
| **Setup Required** | Linked through the signed wallet consent (`provider wallet sign`); the legacy `POST /sn/wallet` request still exists. | Requires a dual-signed fleet binding (`provider fleet bind`; `provider bind-head` remains for compatibility). |
| **Lifecycle Tournament** | **The Baseline On-Ramp:** Guaranteed floor without UID registration burn. | **The Merit Apex:** Earns directly while in the top 200. If IP breadth falls below the cutoff, it is evicted by Yuma churn and **falls back into the pool**. |

---

## 2. Wallet Consent & Coldkey Registration (`provider wallet`)

Settlement pays only a wallet whose coldkey signed the network consent. The consent is one signature that covers every provider client of your network, current and future. `provider wallet status` shows what is stored: the consent coldkey and its epoch interval, whether it is in effect now, still starts in an epoch, or has ended. A legacy login-proof wallet, if any, is listed apart: it is not a consent and settlement does not pay it.

### Signing the consent from your own computer

`provider wallet sign <coldkey_ss58>` serves one local page (127.0.0.1 only) where a browser wallet extension signs the consent. The page counts down 15 minutes and can be extended. Any extension that exposes the Polkadot `signRaw` API works (Talisman, SubWallet, Polkadot{.js}, Nova); the message starts with `Approve URnetwork network wallet mapping`, and the seed phrase never leaves the extension. Run it on your own computer, never on a fleet node. `urnet-tools wallet sign` runs the same flow and refuses on a fleet node unless you pass `--i-am-on-my-own-workstation`.

### Signing without a browser

`provider wallet challenge <coldkey_ss58>` prints the exact consent statement and the epoch interval it covers, and signs nothing. Sign the message with the coldkey outside the tool (a Polkadot extension `signRaw` of type bytes works), then submit it:

```bash
provider wallet set <coldkey_ss58> --message=<text> --signature=<hex>
```

`provider wallet set` with `--coldkey_seed_file=<path>` reads a coldkey seed from disk instead. That form is for your own workstation, never a fleet node, and the `urnet-tools wallet set` form refuses there unless you pass `--i-am-on-my-own-workstation`. The seed file must be a private regular file the caller owns (mode `0600` or read-only `0400`), and the tool never creates one.

### The epoch window

By default the consent covers the next epoch through the full 65,536-epoch span. Use `--wallet-from-epoch` and `--wallet-through-epoch` for an explicit interval; the window spans at most 65,536 epochs, and a later consent must start at a later epoch than the consent it replaces (the consent cannot be moved earlier). An odd interval that would leave epochs unpaid is refused with the reason, unless you deliberately override it:

```text
wallet consent: --wallet-from-epoch=<n> is far beyond the current epoch <m>; epochs before it would be unpaid and the consent cannot be moved earlier (URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1 overrides)
wallet consent: the interval covers <n> epochs, not the full <m>; epochs after it would be unpaid (URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1 overrides)
```

### Legacy wallet registration (compatibility)

The pre-consent registration paths still exist, but they send an unsigned request. The platform may refuse it, and it is not a consent, so settlement does not pay it.

Single-line curl (the same unsigned request the legacy CLI path sends):

```bash
curl -s -X POST "https://api.bringyour.com/sn/wallet" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $(cat ~/.urnetwork/jwt)" \
  -d '{"coldkey_ss58": "5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5"}'
```

Returns `{}` on success.

CLI: `provider wallet set` and `provide --wallet=<coldkey>` send the unsigned network wallet request only with `--legacy-network-wallet`; without it, `wallet set` exits 1 and `provide` logs the refusal and keeps providing.

```bash
provider wallet set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5 --legacy-network-wallet
```

Docker deployments pass the coldkey the same way:

```yaml
services:
  provider:
    image: ghcr.io/full-bars/urnetwork-3.23-fix:v3.23.0-fix.33.0
    command: ["provide", "--wallet=5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5", "--legacy-network-wallet"]
    volumes:
      - ur_config_1:/home/urnet/.urnetwork
```

> [!NOTE]
> Whether the platform still accepts the unsigned request is a server-side policy. If it is refused, set the wallet with the signed consent above, or in the URnetwork app or web account.

### What to check after

`provider wallet status` (or `urnet-tools wallet status`): after signing, the stored consent shows the coldkey and its interval; while the interval has started, it is the wallet settlement pays.

---

## 3. Hotkey Delegation (`provider wallet hotkey`)

One coldkey signature pays the miner on every operator. `provider wallet hotkey` maps the hotkey to the coldkey with a global consent that both keys sign; the consent is kept as a chain of generations under `hotkey-wallet/` in the provider state directory, and the operator stores the chain and a delegation of the miner's network to the chain's head. The coldkey never signs per operator.

- `provider wallet hotkey challenge <coldkey_ss58> --hotkey_seed_file=<path>` builds the next consent statement and prints the exact message for an offline coldkey signature; the statement waits in `hotkey-wallet/pending.json`.
- `provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path>` takes the coldkey signature with `--coldkey_seed_file`, or verifies `--message` and `--signature` against the pending statement; then it signs with the hotkey, stores the generation, and submits the chain and the delegation to the operator this provider resolves (its saved network config, otherwise the default platform URL). A failed submission is reported and the command exits 1; setting the same message and signature again resends it and renews the window. Run it from each provider you run, so every operator you serve stores the chain.
- A delegation starts two epochs after it is signed and is not retroactive. Sign early if an epoch matters.
- `provider wallet hotkey status` prints the chain (each generation and the head), the pending statement while one waits, and the delegation the resolved operator holds: whether it adopts the head, the earning window, and any caveat (a window that ends before the operator's epoch; an operator-side signed network consent that outranks the delegation).

The full reference, including the offline signing rules, the generation and epoch-window rules and the status output, is in [Bittensor hotkey delegation](Bittensor-Hotkey-Delegation.md).

---

## 4. Claiming by Hand (`provider claim`)

Subnet 25 uses cryptographic Merkle tree payout roots committed on-chain at the conclusion of each epoch.

### Epoch Stages
1. **Active Epoch ($e$)**: Nodes stream billable bandwidth; validators collect signed trails.
2. **Commit Window**: Validator consensus commits the payout root hash.
3. **Dispute / Finalization**: Payout root is locked on-chain (`finalize_block`).
4. **Claim Window**: Verified claims become redeemable.

### Claim credential (required)
`provider claim` authenticates with the login of a client that served traffic, because only such a client has a payout. Pass exactly one of:

| Flag | Credential |
|---|---|
| `--store-client=<key>` | The client token of one identity in `~/.urnetwork/.client_jwts.json`. The key is a proxy address or `direct`. A bare address also finds the single credentialed identity at that address; when several proxy accounts share it the command refuses and asks for the full key (`address`, a `\x1f` byte, then the user). Pick an identity that served traffic. |
| `--provider-jwt=<path>` | A client token in a file. |
| `--legacy-coldkey=<coldkey_ss58>` | The network token plus this coldkey. Only for an epoch without a provider artifact. |

The network token alone is refused, and so is an expired client token or (with `--legacy-coldkey`) an expired network token (start the provider so it renews, or pass a fresher file). A command with no credential flag exits 1 and names these options. The examples below show `--store-client=direct`; substitute your own credential flag.

### Workflow 1: Air-Gapped / Offline Calldata (Recommended)
Generates ABI-encoded calldata and cryptographic inclusion proofs without exposing private keys on the provider host:
```bash
provider claim --store-client=direct --epoch=1053
```
Output yields ready-to-submit calldata for `provider snclaim submit --calldata=<hex>` (see [the low-level on-chain tool](#the-low-level-on-chain-tool-provider-snclaim) below) or for a web3 wallet.

### Workflow 2: Direct On-Chain Submission
Submit the claim transaction directly with a local private key file and RPC endpoint:
```bash
provider claim \
  --store-client=direct \
  --epoch=1053 \
  --rpc=https://rpc.subtensor.network \
  --key_file=/path/to/coldkey_evm.key
```

### Dry Run Verification
Simulate verification and root matching without broadcasting transactions:
```bash
provider claim --store-client=direct --epoch=1053 --rpc=https://rpc.subtensor.network --dry-run
```

### The low-level on-chain tool (`provider snclaim`)
`provider snclaim` is the settlement-vault submission tool that sn ships as a separate `snclaim` binary. It is built from the same library the claim daemons use, and it is reachable here as a subcommand instead of a second binary. Everything after `snclaim` is passed to it unchanged, so `provider snclaim --help` prints its own usage (which names its commands `snclaim ...`). `provider claim` and the claim daemons stay the normal way to claim; use `snclaim` when they do not fit.

- `provider snclaim submit` signs and broadcasts a claim transaction, either from prepared calldata (`--calldata=<hex>`, for a key kept off the provider host) or from the claim fields (`--epoch`, `--no_id`, `--coldkey`, `--share_bps`, `--proof`).
- `provider snclaim status` reads the state of one claim.
- `provider snclaim v2 status`, `v2 claim` and `v2 check` are the settlement v2 counterparts.
- `provider snclaim withdraw` retries a v1 claim whose payment was deferred because it was below the transfer minimum, no alpha price was available, or the runtime transfer failed. The v1 vault keeps such a claim as a credit for the coldkey, and anyone may retry it at any time. Payment always lands on the coldkey, and the key file you pass only pays gas. Only the v1 vault holds credit: the v2 vault pays a claim in full or reverts it, so `withdraw` takes `--vault=v1` and refuses anything else.

```bash
provider snclaim withdraw --vault=v1 --coldkey=<coldkey> --contract=<v1 vault address> --rpc=<evm rpc url> --key_file=<gas key file> --dry-run
```

Without `--dry-run` the transaction is sent. A withdrawal the vault would revert fails the preflight with the decoded reason, and a coldkey with no credit is refused before anything is signed.

If claims should not wait for hand runs, run a daemon alongside the provider: [Bittensor claim daemons](Bittensor-Claim-Daemons.md).

---

## 5. Claim Daemons (`provider claim-daemon`, `provider claim-daemon-v2`)

Both daemons keep claims submitted as epochs finalize, so no one has to run `provider claim` per epoch by hand. Each takes a strict YAML config file (`--config=<path>`); relative paths inside the config are anchored at the config file.

- `provider claim-daemon` runs the settlement-v1 claim loop. It reads the retained claim credential (`jwt_file` when the config sets one, otherwise the provider's own token); while that credential is missing, empty or no longer accepted, it prints one line to stderr (`claim-daemon: <reason>; driving retained signed work only`) and stops taking new work, while still driving entries whose exact bytes were already signed.
- `provider claim-daemon-v2` runs the settlement-v2 claim loop and needs no operator credential: it reads the finalized accounts from `api_url` and checks each account's Merkle proof against the vault root at a finalized block. `--v1-config=<path>` runs the unchanged v1 daemon beside it in one process; `--threshold-rao` claims an account once its balance reaches the value (default 1,900,000,000 rao); `--claim-now` claims everything claimable once at start.
- Both require an explicit durable-volume declaration (`--durable-volumes=<path>` with `--durable-volumes-sha256=<hash>`) and a private state directory. Signed transactions are journaled under the state directory before broadcast, so already signed entries survive a restart and reconcile without a credential.

The full config reference key by key, the state directory rules, the finality checks and the restart behavior are in [Bittensor claim daemons](Bittensor-Claim-Daemons.md).

---

## 6. Fleet (Head Tier) (`provider fleet`)

The fleet group drives the head-tier lifecycle for one fleet node from a single canonical manifest: the `urnetwork-fleet-manifest-v1` JSON (`--manifest=<path>`) is the single source for the fleet identity.

- `provider fleet manifest` prints the manifest's canonical form, its commitment sha256 and its member count.
- `provider fleet register` registers the manifest hotkey on the manifest netuid (the fleet's own burned registration), signed by the fleet coldkey. It is a plan unless `--apply` is given: it reads the live burn, refuses a burn above `--burn_limit_rao` (default: the burn observed at the finalized read), quotes its fee and broadcasts nothing.
- Mainnet writes need the reviewed runtime authority pin (`--mainnet-runtime-authority` with `--mainnet-runtime-authority-sha256`) and a durable-volume declaration; every broadcast is journaled under the provider state, and replaying a retained native write requires `--apply`.
- `provider fleet publish` writes the manifest commitment on chain (it always writes; there is no plan flag).
- `provider fleet bind` is the dual-signed member binding through the EVM relayer (`--relayer_key_file` receives no binding ownership); `--dry-run` stops at the preflight.
- `provider fleet sign` prints the payload the Top 200 submission page takes, plus the hotkey signature; it uses no RPC and never prints a seed.
- `provider fleet status` reads the chain back and checks it against the canonical manifest; `provider fleet revoke` ends a binding at a future `--effective_epoch`.
- Keep the write commands and their seed files on a workstation or a locked-down host where possible, never a mining host.

The full reference, including the manifest fields, the runtime authority pin, the recovery journal and the seed-file rules, is in [Bittensor fleet](Bittensor-Fleet.md).

### Legacy head-node binding (`bind-head`, `unbind-head`)

Head nodes aggregate proofs from client nodes for on-chain batch verification.

> [!NOTE]
> `bind-head` and `unbind-head` are unchanged in this release (not removed, no rename). They are the pre-fleet EVM-only path, kept for compatibility; new work should use the `provider fleet` group above.

```bash
provider bind-head \
  --manifest=/path/to/fleet-manifest.json \
  --hotkey_seed_file=/path/to/hotkey.seed \
  --valid_from_epoch=<n> \
  --valid_to_epoch=<n> \
  --rpc=<rpc_url> \
  --key_file=/path/to/evm.key
```
The manifest is the single source for the fleet identity. Without `--key_file` the command prints the ready-to-submit calldata for the offline path; with it, the signed binding is submitted through `--rpc`. `--client_id` and `--client_seed_file` override the provider's own client identity for the binding signature; `--dry-run` signs but sends nothing.

> [!IMPORTANT]
> The fleet binding is dual-signed: the sr25519 hotkey seed must match the manifest hotkey, and the Ed25519 client side signs with the provider's own client key. Both signatures are checked against the same digest.

```bash
provider unbind-head \
  --manifest=/path/to/fleet-manifest.json \
  --effective_epoch=<n> \
  --rpc=<rpc_url> \
  --key_file=/path/to/evm.key
```
`--effective_epoch` is the future epoch at which the revocation takes effect.

---

## 7. Operators (`provider operators`, `provide --all-operators`)

The operator surface runs one provider per operator in the operator list.

- `provider operators` prints each listed operator with its credential state (`jwt present` or `awaiting auth`), and the list's source and digest.
- `provider provide --all-operators` runs one provider per operator, each with its operator directory as home and its provider state under `.urnetwork` inside it. The network token the supervisor watches is `operators/<domain>/.urnetwork/jwt`; an operator without it is reported as `awaiting auth`, gets no child, and is rechecked every 5 minutes, so it starts as soon as its token is written.
- The list is fetched from `--operators-url` (https, or http only for loopback; omitted, the built-in default list); the last good copy is kept as `operators.yml` in the provider state directory, and `--operators-refresh` sets the refetch cadence (a Go duration of at least 1m; default 1h). `--api_url` is refused with `--all-operators`.

The state layout, the trust model of the list and the operating notes are in [Multi-operator providers](Multi-Operator.md).

---

## 8. Real-Time Status & Telemetry (`sn-status`)

Use `sn-status` to inspect your global ranking, bandwidth delivery, coldkey linkage, and Top 200 cutoff eligibility in real time without exposing credentials in the process table.

### Commands
```bash
# Host CLI (systemd / bare-metal)
urnet-tools sn-status

# Docker CLI (delegates directly into running container)
urnet-docker sn-status

# Provider binary directly
provider sn-status

# JSON format for monitoring agents and Prometheus scrapers
urnet-tools sn-status --json
```

### Dashboard Output
```text
════════════════════════════════════════════════════════════════════════════════
  URNetwork Subnet 25 - Node & Miner Status
════════════════════════════════════════════════════════════════════════════════
  Provider:         urnetwork.service (State: /var/lib/urnetwork)
  Network:          miner-node-01 (f078e470-36d0-48e0-bb15-b5d1e2e9c1aa)
  Global Rank:      #7 [Public] - Tier 1 Elite (Rank #7 Globally)
  Net Bandwidth:    1,234,567.89 MiB (1205.63 GiB) provided
  Coldkey (SS58):   5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
  Coldkey (Hex):    0xa21b...
  Subnet Epoch:     #1054 (Start: #53121600, Finalize: #53172000)
  Contract:         0x5FbDB2315678afecb367f032d93F642f64180aa3 (Chain ID: 964)
  Payout Share:     4.85% (485 bps in Epoch #1053)
════════════════════════════════════════════════════════════════════════════════
```

### Ranking Tiers & Eligibility
- **Tier 1 Elite (Rank 1-10)**: Top global emission allocation; highest validator query preference.
- **Tier 2 High-Volume (Rank 11-50)**: High yield; fast validator probe acceptance.
- **Tier 3 Active (Rank 51-200)**: Subnet 25 emission cutoff eligibility boundary.
- **Rank 201+**: Displays the exact distance to the Top 200 cutoff (`Rank #N (M below Top 200 cutoff)`).
- **No rank yet**: Displays `Unranked / Processing` until the platform has initial telemetry for the node.

---

## 9. Troubleshooting

Most Bittensor failures are a missing credential or a protection doing its job. The message names what the command wants; the common ones:

| Message or symptom | What it means and what to do |
|---|---|
| `claim needs the login of a client that served traffic: pass --store-client=<key> ...` | `provider claim` needs exactly one credential, and it must be an identity that served traffic. Keys are in `~/.urnetwork/.client_jwts.json`. |
| `pass only one of --store-client, --provider-jwt and --legacy-coldkey` | Give exactly one credential flag. |
| `no client "<key>" in <path> (N identities) ...` | The store has no identity under that key. List the store for the right key. |
| `N identities in <path> share the address "<addr>" ...` | The address is ambiguous. Pass the full key (`address`, a `\x1f` byte, then the user). |
| `the network jwt has expired; run provider auth again, then retry` | The legacy network token is stale. Start the provider so it renews, or pass a fresher file. |
| `wallet set` exits 1, or `provide` logs a refusal | The unsigned wallet request is refused under the platform's signed wallet rules. Sign the consent (section 2), or pass `--legacy-network-wallet` to send it anyway. |
| `claim-daemon: <reason>; driving retained signed work only` | The v1 credential is missing, empty or no longer accepted. Fix the token; new work pauses and retained signed entries still finish. With nothing retained signed, the daemon exits 1. |
| A daemon exits 1 at startup with a `claim-daemon:` or `claim-daemon-v2:` prefix naming a check | Fix that input (config, durable declaration, key file, batcher, threshold); the prefix names the failing check. |
| `explicit durable-volume declaration and hash are required` | Pass both `--durable-volumes` and `--durable-volumes-sha256`; fleet mainnet writes and the claim daemons require a matching declaration. |
| `fleet status does not match the canonical manifest` | The chain state differs from the manifest (a missing publish or binding, or a newer generation). Re-check, re-run the write, then status again. |
| `N of M authenticated operators do not adopt the head yet; set it again with the same --message and --signature` | The submission failed at the operator it resolved. Resend the identical message and signature to retry; the resubmission also renews a stale delegation window. |
| `not effective: the window ends at epoch A and the operator is at epoch B; the next provide upkeep renews it` | The delegation window ended. A running provider renews it on its hourly upkeep; setting the same message and signature again also renews it. |
| `private seed file custody is unsupported on this platform` | Seed-bearing commands (`wallet hotkey`, the `fleet` writes) run on Linux or macOS only. |
| A state directory is refused (not private, not owned, not a regular file, or already owned) | Fix permissions and ownership; run one daemon per state directory. |
| `wallet consent: --wallet-from-epoch=...` refusals | The interval would leave epochs unpaid. Use the default window, or set `URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1` to override. |

The fuller message tables are in [Troubleshooting](Troubleshooting.md).

---

## 10. FAQ

### Why didn't my browser wallet pop up when I linked my wallet?

The legacy link (the `POST /sn/wallet` request, `provider wallet set --legacy-network-wallet`, `provide --wallet=`) only records a payout address. Sharing a public address cannot move funds, so no signature is needed and no popup appears. The signed consent paths do open the browser: `provider wallet sign` (or `urnet-tools wallet sign`) serves the local signing page. If you expected a popup, you ran the legacy path; use `provider wallet sign`.

### Which wallet signs, and what can the signature do?

The consent is one signature from your coldkey that covers every provider client of your network, current and future. The seed phrase never leaves the browser extension, and the extension shows exactly what it signs. The signature cannot move funds; it decides which wallet settlement pays.

### Do I have to do anything after upgrading?

Nothing here runs unless you start it, and nothing is required for a node to keep providing. The optional order: sign the network wallet consent (section 2); if you mine through a hotkey, sign the hotkey delegation early because it starts two epochs after signing (section 3); claim settled epochs by hand (section 4) or run a claim daemon (section 5); register a fleet only if you run a head node (section 6).

### When does a hotkey delegation start earning?

Two epochs after the delegation is signed, and never retroactively: epochs already under way are not covered. If an epoch matters, sign early.

### Do I need a Subnet 25 UID to earn?

No. Pool providers operate as a `client_id` inside an operator's pool with no UID; a head fleet holds its own UID and earns directly while it stays in the top 200. Section 1 compares the two tiers.

### What happens to claims while a claim daemon is stopped?

Nothing is lost: already signed entries are journaled with their exact bytes and reconciled after a restart, and entries that were not signed are retried. The daemon never replaces a retained signed transaction; it proves what happened first. See [Bittensor claim daemons](Bittensor-Claim-Daemons.md).

---

## 11. Operational Security Guidelines

1. **Never pass tokens via CLI flags**: Avoid passing tokens in process flags (`-jwt=...` or `-pass=...`) where they are visible in `ps aux` or `/proc/<pid>/cmdline`.
2. **File Permissions**: Ensure `~/.urnetwork/jwt` and private keys are set to `0600` permissions.
3. **Container Isolation**: Ensure each container mount uses a dedicated Docker volume (e.g. `ur_config_1`, `ur_config_2`) to prevent state corruption.
4. **Seed files**: the commands that read private seed files (`wallet hotkey`, the `fleet` writes, `wallet set --coldkey_seed_file`) want a private file the caller owns (mode `0600` or read-only `0400`), and the tool never creates one. Keep fleet coldkey seeds off mining hosts where possible, and never paste seed material into a command line or a shared file.

---

## 12. Third-party licenses

Provider builds from this repository statically link the vendored substrate RPC client, and the repository keeps one inventory for everything third-party: `THIRD_PARTY_LICENSES.md` lists each component, its license, its vendored path and its license files. The note worth repeating here is the LGPL one: inside `third_party/go-substrate-rpc-client`, the `gethrpc` package is LGPL-3.0 (its license files are kept in place there), while the rest of that tree is Apache-2.0, and the complete source of the linked version is in this repository at the vendored path.
