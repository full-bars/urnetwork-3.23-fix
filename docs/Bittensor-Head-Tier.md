# Bittensor Head Tier (Optional and Advanced)

> [!IMPORTANT]
> **Most operators should not do this.** Not doing it costs you nothing. If you have signed the network wallet consent (see the [Quick Start](Bittensor-Quick-Start.md)), your nodes already earn in the pool. If you are not sure you want the head tier, you do not want it.

> [!NOTE]
> Checked against upstream sn `83e6f48c2`, server `50851997d`, connect `f1837db6` and sdk `23b3c7dd` (2026-10-21). The numbers on this page come from the approved mainnet policy (`deploy/mainnet/policy-v1.yml` in sn). Upstream is still changing this area quickly, so re-check [Current blockers](#current-blockers) before you spend anything.

## Should I even consider it?

Answer all five. One "no" means stay in the pool.

1. Do you control **many** public IP ranges that are distinct (different /29 for IPv4, different /48 for IPv6), stable and not shared with anyone else?
2. Are you willing to burn TAO for a seat you can lose, with no refund?
3. Can you keep seed files on your own computer and never on a node?
4. Can you re-sign every member's binding about every 3 months?
5. Are the [current blockers](#current-blockers) solved for you?

## What it is

Subnet 25 has 256 seats (called UIDs). URnetwork's whole pool uses one seat. Up to 200 more seats are for "head fleets" (the "Top 200", `maximum_head_fleets: 200` in the policy).

A fleet is one hotkey plus a list of your node identities bound to it. Validators measure how many **different public IP ranges** your bound nodes really served traffic through. The ranges are /29 for IPv4 and /48 for IPv6 (`egress_ipv4_prefix: 29`, `egress_ipv6_prefix: 48`). A range claimed by two fleets is split equally between them. How much traffic you carry does not matter. How many different ranges you cover does.

A fleet is paid directly by the network to its hotkey, every tempo (about 72 minutes). There is no claim step. A node earns in only one tier at a time:

- A bound node leaves the pool payout.
- If the fleet loses its seat, or a binding expires, its nodes fall back to the pool from the next epoch.

## Who it is for

Operators with **many distinct, stable public IP ranges** that are not shared with other fleets and do not rotate. Upstream puts it this way: "A single residential provider scores about 1 prefix, so the head is in practice for operators of many routable IPs." Shared or rotating proxy exits are a poor fit: a shared range is split, and a rotating one does not stay yours.

## What it costs and requires

The live figures below are read from the chains by CI, so this page does not carry hand-maintained numbers. The bullets after the table explain what each figure means.

<!-- BEGIN SN25 LIVE NUMBERS (generated) -->
_Live values · last changed 2026-10-22 19:07 UTC · checked hourly_

| | Value |
|---|---|
| Fleet seat registration burn | 0.0005 TAO (~$0.13) |
| Burn floor | 0.0005 TAO |
| Seats in use | 256 of 256 |
| Registration open | yes |
| Immunity after registering | 21,600 blocks (~3.0 days) |
| Tempo | 360 blocks (~72 minutes) |
| Gas price, chain 964 | 5.3 gwei |
| One node binding | 245,301 gas, about 0.001292 TAO (~$0.33) |
| Commitment oracle | active, balance 0.0479 TAO |
| Coordinator | not paused |
<!-- END SN25 LIVE NUMBERS (generated) -->

- **A registration burn.** Your fleet coldkey burns TAO to register the fleet hotkey's seat. The amount is live. `fleet register` reads it, prints it, and refuses above the limit you set with `--burn_limit_rao`. It only plans until you add `--apply`.
- **You can lose the seat.** Nothing reserves a place for head fleets. When the subnet is full, a new registration removes the lowest-scoring seat that is past its immunity period. The live table above carries the current seat count and immunity length. A lost seat means the burn is gone.
- **Gas for each node you bind.** `fleet bind` sends one EVM transaction per node through a relayer key you fund. The other route is `fleet sign`, which signs locally and needs no relayer, and then you submit on the ur.io "Top 200" page. The server side of that page exists (`POST /sn/head/binding`). It is not confirmed that the page itself is live for operators.
- **Re-signing.** A binding lasts at most 12 epochs (`maximum_validity_epochs: 12`, about 3 months). Changes take effect from the next epoch.
- **Several keys**, all as files on your own computer:
  - a fleet coldkey seed (sr25519). It pays the burn. Keep it off every node.
  - a fleet hotkey seed (sr25519).
  - one **different** Ed25519 client key per bound node.
  - an EVM relayer key, only if you use `fleet bind` or `fleet revoke`.
- **A durable-volume declaration** for mainnet writes (see step 2).
- **Linux or macOS** for any command that reads a seed file. Seed files must be private files (mode `0600`).

## Order of steps

All commands below are in this build's `provider` program. Run them on your own computer, never on a mining node. Full flag details are on the [Fleet page](Bittensor-Fleet.md).

0. **Keep the network consent signed.** It covers every node you do not bind, except a node with its own per-provider consent, which wins for that node.

1. **Write the manifest.** This is the fleet file. This build has no `fleet init` command, so write it by hand. The fields are listed on the Fleet page under [The manifest](Bittensor-Fleet.md#the-manifest). In short: schema `urnetwork-fleet-manifest-v1`, chain 964 (mainnet), subnet 25, the coordinator address, the fleet id, the hotkey, a nonzero generation, and the members (`client_id` plus `client_key`, each used once). Then check it:
   ```bash
   provider fleet manifest --manifest=<path>
   ```
   It prints the canonical form, its commitment hash and the member count. Your file must match the canonical form byte for byte. Do not edit a manifest in place later: changing members means a new generation and a new publish.

2. **Register the seat.** Run it first as a plan, then again with `--apply`:
   ```bash
   provider fleet register --manifest=<path> --hotkey_seed_file=<path> --coldkey_seed_file=<path> --substrate=<ws_url> \
     --burn_limit_rao=<n> [--fee_limit_rao=<n>] \
     --mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex> \
     --durable-volumes=<path> --durable-volumes-sha256=sha256:<64 hex>
   ```
   The plan prints the live burn, your balances, the enforced limit and the quoted fee, and broadcasts nothing. The runtime-authority pair (`--mainnet-runtime-authority`, `--mainnet-runtime-authority-sha256`) and the durable-volume pair are needed for every mainnet write (`register`, `publish`, `bind`, `revoke`) and for mainnet `status`. Without the durable-volume pair a mainnet write stops early with `explicit durable-volume declaration and hash are required`.

3. **Publish the commitment.** This always writes. There is no plan mode:
   ```bash
   provider fleet publish --manifest=<path> --substrate=<ws_url> --hotkey_seed_file=<path> [mainnet flags as above]
   ```
   Publish again whenever the manifest generation changes.

4. **Bind each node.** Either send it yourself (`--dry-run` stops before sending):
   ```bash
   provider fleet bind --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> \
     --valid_from_epoch=<e> --valid_to_epoch=<e> --rpc=<rpc_url> --relayer_key_file=<path> [--dry-run] [mainnet flags]
   ```
   Or sign locally and submit on the Top 200 page:
   ```bash
   provider fleet sign --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> \
     --valid_from_epoch=<e> --valid_to_epoch=<e>
   ```
   The window from `valid_from_epoch` to `valid_to_epoch` can be at most 12 epochs.

5. **Check it.** This only reads:
   ```bash
   provider fleet status --manifest=<path> --client_id=<hex> --substrate=<ws_url> --rpc=<rpc_url> [mainnet flags]
   ```

6. **Rebind before the recorded `valid_to_epoch`,** with a few days of margin. Put the date in your calendar when you bind. An expired binding returns the node to the pool.

## Leaving the head tier

Revoke each member at a future epoch:

```bash
provider fleet revoke --manifest=<path> --client_id=<hex> --client_seed_file=<path> --effective_epoch=<e> \
  --rpc=<rpc_url> --relayer_key_file=<path> [--dry-run] [mainnet flags]
```

Or let the binding run out at its `valid_to_epoch`. Either way the node goes back to the pool from the next epoch, and your network consent pays it again, unless the node has its own per-provider consent, which keeps paying to that consent instead. The burn is not refunded.

## What can go wrong

| Symptom | Cause | What to do |
|---|---|---|
| Your seat is gone after you paid the burn | A newer registration pushed out the lowest-scoring seat | Nothing recovers the burn. Your nodes are back in the pool. |
| `bind` reverts with `StaleCommitment()` | The commitment oracle has not copied your commitment to the EVM chain. See [Current blockers](#current-blockers). | Wait for the oracle. Do not try to mirror it by hand. |
| `fleet status` says the commitment is untagged | It was published before the `urfleet1` format (2026-10-15) | Run `fleet publish` again. Old mirrored fleets keep binding only until about block 9,360,673. |
| The manifest is refused | A client key or client id is used twice, or the file is not in canonical form | Give each member its own key, and copy the form `fleet manifest` prints. |
| Your score is lower than your range count | Another fleet claims some of the same ranges, so they are split | Use ranges only you control. |
| A node dropped out of the head | Its 12-epoch window lapsed | Bind it again. It earns in the pool until then. |

## Current blockers

> [!WARNING]
> As of sn `83e6f48c2` (2026-10-21):
> - **The commitment oracle is not deployed.** Upstream's status (sn `mainnet/LAUNCH.md`) says the service is built but not deployed, so no fleet commitment is mirrored. The service key `0x56Dd…CF03` is already the coordinator's active oracle and holds a small balance, so the wiring is in place; until the service runs, every `bind` fails with `StaleCommitment()`. Check before you spend anything.
> - **One client per provider install.** The provider keeps one `.provider.key` per state directory, and every proxy slot on it signs with that key, so one install can bind one member. A box that needs more members runs one install per member, each with its own `URNETWORK_STATE_DIR`. Per-slot keys are a design idea, not built.
> - **No mainnet authority file is shipped.** Mainnet `register`, `publish`, `bind`, `status` and `revoke` need a runtime authority file and its approved hash. Where an ordinary operator gets this file is **not confirmed**.
> - **Old fleets must republish.** Fleets published before 2026-10-15 hold an untagged commitment that the oracle never mirrors. Republish with `provider fleet publish`.

If any of these is a problem for you, stay in the pool. You lose nothing.
