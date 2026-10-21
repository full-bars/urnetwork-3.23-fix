# Bittensor Head Tier (Optional and Advanced)

> [!IMPORTANT]
> **Most operators should not do this.** Not doing it costs you nothing. If you have signed the network wallet consent (see the [Quick Start](Bittensor-Quick-Start.md)), your nodes already earn in the pool. If you are not sure you want the head tier, you do not want it.

## What it is

Subnet 25 has 256 seats (called UIDs). URnetwork's whole pool uses one seat. Up to 200 more seats are for "head fleets" (the "Top 200").

A fleet is one hotkey plus a list of your node identities bound to it. Validators measure how many **different internet address ranges** your bound nodes really served traffic through. The ranges are /29 for IPv4 and /48 for IPv6. A range shared with other fleets is split between them. How much traffic you carry does not matter. How many different ranges you cover does.

A fleet is paid directly by the network to its hotkey, every tempo (about 72 minutes). There is no claim step. A node earns in only one tier at a time. A bound node leaves the pool payout. If your fleet loses its seat, its nodes fall back to the pool.

## Who it is for

Operators with **many distinct, stable public IP ranges** that are not shared with other fleets and do not rotate. Upstream puts it this way: a single residential provider scores about 1 range, so the head tier is in practice for operators of many routable IPs. Shared or rotating proxy exits are a poor fit.

## What it costs and requires

No figures are given here, because they change.

- **A registration burn.** Your fleet coldkey burns TAO to register the fleet hotkey's seat. The amount is live. The command reads it and refuses above the limit you set with `--burn_limit_rao`. It only shows a plan until you add `--apply`.
- **You can lose the seat.** There is no rule that protects the top 200. When the subnet is full, a new registration removes the lowest-earning seat. A lost seat means the burn is gone.
- **Gas for each node you bind.** Binding sends one blockchain transaction per node, through a relayer key you fund. The amount is not documented. The other route is to sign locally and submit on the ur.io "Top 200" page. It is not confirmed that this page is live.
- **Re-signing.** Bindings last at most 12 epochs (about 3 months) on mainnet. Changes take effect the next epoch.
- **Several keys:** a fleet coldkey (pays the burn, keep it off nodes), a fleet hotkey seed file, one different Ed25519 key per bound node, and a relayer key if you use `fleet bind`.
- **Linux or macOS** for any command that reads a seed file.

## Prerequisites checklist

1. The network wallet consent is signed. It covers every node you do not bind, except a node with its own per-provider consent, which wins for that node.
2. A coldkey with enough TAO for the live burn, and a separate hotkey seed file.
3. Nodes with stable, distinct egress IP ranges.
4. One unique client key per bound node (see the blockers below).
5. The mainnet runtime authority file and its approved hash (see the blockers below).

## Order of steps

All commands below are in this build's `provider` program. Seed files must be private files (mode `0600`) and never go on a mining node.

1. **Build the manifest.** This is the fleet file that lists the hotkey, the coordinator, chain 964, subnet 25, the generation and the members. This build has no `fleet init` command, so write the file by hand. Then check it:
   ```bash
   provider fleet manifest --manifest=<path>
   ```
   It prints the canonical form, its commitment hash and the member count.
2. **Register the seat.** First run it as a plan only, then again with `--apply`:
   ```bash
   provider fleet register --manifest=<path> --hotkey_seed_file=<path> --coldkey_seed_file=<path> --substrate=<ws_url> [--burn_limit_rao=<n>] [--fee_limit_rao=<n>]
   ```
   Mainnet writes also need `--mainnet-runtime-authority`, `--mainnet-runtime-authority-sha256`, `--durable-volumes` and `--durable-volumes-sha256`.
3. **Publish the commitment** (this always writes):
   ```bash
   provider fleet publish --manifest=<path> --substrate=<ws_url> --hotkey_seed_file=<path>
   ```
4. **Bind each node.** Either send it yourself:
   ```bash
   provider fleet bind --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> --valid_from_epoch=<e> --valid_to_epoch=<e> --rpc=<rpc_url> --relayer_key_file=<path> [--dry-run]
   ```
   Or sign locally and submit on the Top 200 page:
   ```bash
   provider fleet sign --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> --valid_from_epoch=<e> --valid_to_epoch=<e>
   ```
5. **Check it:**
   ```bash
   provider fleet status --manifest=<path> --client_id=<hex> --substrate=<ws_url> --rpc=<rpc_url>
   ```
6. **Rebind before the recorded `valid_to_epoch`,** with a few days of margin: a binding lasts at most 12 epochs (about 3 months) and an expired binding returns the node to the pool. **To leave,** revoke:
   ```bash
   provider fleet revoke --manifest=<path> --client_id=<hex> --client_seed_file=<path> --effective_epoch=<e> --rpc=<rpc_url> --relayer_key_file=<path>
   ```

Full flag details are on the [Fleet page](Bittensor-Fleet.md).

## What can go wrong

- You burn TAO and a newer registration removes your seat.
- A bind fails with `StaleCommitment()` if the commitment oracle has not copied your commitment yet.
- An old, untagged commitment must be published again. `fleet status` tells you.
- A reused client key makes the manifest get refused.
- Overlapping IP ranges with other fleets split your score.
- If the 12-epoch window lapses, the node drops out of the head and goes back to the pool from the next epoch.
- Upstream is still changing this area.

## Current blockers (honest status)

> [!WARNING]
> - **One client per node.** This build uses one `.provider.key` for every proxy client on a box. Today at most **one client per node** can be a fleet member. Per-slot keys are only a design idea, not built.
> - **No mainnet authority file is shipped.** Mainnet `register`, `publish`, `bind`, `status` and `revoke` need a runtime authority file with an approved hash. Where an ordinary operator gets this file is **not confirmed**.
> - **The commitment oracle.** Binds only work if it is running. Its status is **unconfirmed**: it was reported as built but not deployed in mid October. Check before you spend anything.
> - Fleets published before 2026-10-15 must publish again in the new format.

If any of these is a problem for you, stay in the pool. You lose nothing.
