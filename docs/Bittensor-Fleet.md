# Bittensor Fleet (Head Tier)

> [!WARNING]
> **Most operators can stop here.** This is the optional, advanced head tier. Not using it costs nothing, and a signed network consent already earns your nodes the pool share. Read [Bittensor Head Tier](Bittensor-Head-Tier.md) first, including its current blockers.


`provider fleet` drives the head-tier lifecycle for one fleet node from a single canonical manifest: the burned registration of the fleet hotkey, commitment publication, the dual-signed client and hotkey binding, local submission signing, status and revocation. It is the group a Top 200 head-tier operator runs.

Read commands are safe to run anywhere. The write commands (`register`, `publish`, `bind`, `revoke`) and every seed file they read belong on a workstation or a locked-down host where possible, never a mining host: the fleet coldkey seed pays the registration burn, and the client and hotkey seeds sign bindings the whole fleet depends on.

The rest of the Subnet 25 operations, including pool claims, wallet mapping and the claim daemons, are in [Bittensor operations](Bittensor-Operations.md).

## What the fleet group is and who runs it

A fleet is a set of independently keyed UR clients under one Bittensor hotkey, run by a head-tier operator who holds the fleet coldkey and the fleet hotkey. The group is the head-tier lifecycle for that fleet: prove the canonical member set (`manifest`), burn the fleet hotkey's own miner UID (`register`), commit the manifest hash on-chain (`publish`), link a member to the hotkey for an epoch window (`bind`), sign locally for the Top 200 submission page (`sign`), read the chain back against the manifest (`status`) and end a member's binding at a later epoch (`revoke`).

The group has no daemon and no background loop: every command is a single run over the manifest, the chains and the local state, and it exits when finished.

## The manifest

Every subcommand reads the same JSON document (`--manifest=<path>`), schema `urnetwork-fleet-manifest-v1`. The manifest is the single source for the fleet identity.

- `schema`: the fixed schema string `urnetwork-fleet-manifest-v1`.
- `chain_id`: the EVM chain id; mainnet manifests are chain 964 and require the reviewed runtime pin below, testnet manifests are chain 945.
- `netuid`: the subnet id.
- `coordinator`: the coordinator contract address, `0x` plus 40 lowercase hex digits.
- `fleet_id`: the 32-byte fleet identity, `0x` plus 64 lowercase hex digits.
- `hotkey`: the fleet hotkey sr25519 public key, 32 bytes as `0x` hex.
- `generation`: the nonzero fleet generation counter.
- `members`: one or more `{"client_id": "0x...", "client_key": "0x..."}` entries with 16-byte ids and 32-byte Ed25519 keys; every id and every key is used once, and no entry may be zero.

The file must be exactly the canonical form: lowercase `0x` hex, members sorted by `client_id`, no unknown fields, no duplicate JSON documents, and one trailing newline at most. `provider fleet manifest` prints that canonical form, its commitment sha256 and its member count.

The commitment sha256 is the value the fleet hotkey writes to Subtensor's commitments pallet; each dual-signed binding carries it together with its own epoch window, so one committed generation can be activated at the next safe coordinator boundary.

> [!NOTE]
> The manifest is immutable input, not configuration to be edited in place. Changing members or the generation means a new generation and a new publish, because the edited file no longer matches the on-chain commitment. Only a formatting or schema violation makes the file fail to parse.

## The command surface

```text
    provider fleet manifest --manifest=<path>
        [-v...]
    provider fleet register --manifest=<path> --hotkey_seed_file=<path> --coldkey_seed_file=<path> --substrate=<ws_url>...
        [--burn_limit_rao=<n>] [--fee_limit_rao=<n>] [--apply | --dry-run]
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet publish --manifest=<path> --substrate=<ws_url>... --hotkey_seed_file=<path>
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet bind --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> --valid_from_epoch=<e> --valid_to_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet sign --manifest=<path> [--client_id=<hex>] [--client_seed_file=<path>] [--hotkey_seed_file=<path>] --valid_from_epoch=<e> --valid_to_epoch=<e>
        [-v...]
    provider fleet status --manifest=<path> --client_id=<hex> --substrate=<ws_url>... --rpc=<rpc_url>...
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet revoke --manifest=<path> --client_id=<hex> --client_seed_file=<path> --effective_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
```

- `provider fleet manifest` prints the manifest's canonical form, its commitment sha256 and its member count. Reads only.
- `provider fleet register` registers the manifest hotkey on the manifest netuid (the head-tier fleet's own burned registration), signed by the fleet coldkey through `--substrate` (repeatable, tried in order). It is a plan unless `--apply` is given: without it, the command reads the live burn economics and the coldkey balance, refuses a burn above the cap, builds and signs the `register_limit` extrinsic, quotes its fee and stops, and broadcasts nothing (`--dry-run` selects the same no-broadcast plan). With `--apply` it submits the write and waits for canonical finality. `--burn_limit_rao` caps the registration burn (default: the burn observed at the finalized read); `--fee_limit_rao` caps the native transaction fee (default 10,000,000 rao, sized above the observed registration fee). Every broadcast is journaled under the provider state, and a retained native write is replayed only with `--apply`. Writes.
- `provider fleet publish` writes the fleet commitment (the manifest commitment hash) to the chain through `--substrate` and verifies the write is finalized. It has no plan flag: publish always writes.
- `provider fleet bind` builds the manifest binding and submits it through the EVM relayer key (`--relayer_key_file`, which receives no binding ownership) via `--rpc`. Without `--dry-run` the transaction is sent and waited on; `--dry-run` stops at the preflight (chain id check, call simulation, gas estimate) and sends nothing. Writes.
- `provider fleet sign` builds the same binding and signs it locally: it prints the payload the Top 200 submission page takes, plus the hotkey signature when `--hotkey_seed_file` is given. It reads only local files, uses no RPC and no relayer key, and never prints or writes a seed. Reads local files only.
- `provider fleet status` reads the on-chain state for `--client_id` (native commitment, coordinator record, epochs, UID) and checks it against the canonical manifest. Reads only.
- `provider fleet revoke` schedules the binding's revocation at `--effective_epoch` (a future epoch), signed by the client key and submitted through `--relayer_key_file`; without `--dry-run` it is sent, and `--dry-run` stops at the preflight and sends nothing. Writes.
- Broadcast defaults in this group: `register` plans (signs and quotes, no broadcast) unless `--apply`; `bind` and `revoke` send unless `--dry-run`; `publish` always writes and verifies finality; `manifest`, `sign` and `status` never write to any chain.

## register in depth

`register` is the one command that creates the fleet's on-chain identity. A plan run prints the authenticated runtime, the live registration economics (burn, min, max, half-life, growth), the hotkey and coldkey with free balance, the enforced burn limit, the signed extrinsic and the quoted fee, then stops:

```bash
provider fleet register \
  --manifest=/path/to/fleet-manifest.json \
  --hotkey_seed_file=/path/to/hotkey.seed \
  --coldkey_seed_file=/path/to/coldkey.seed \
  --substrate=<ws_url>
```

The plan refuses to continue when the hotkey is already registered under a different coldkey, when the live burn exceeds `--burn_limit_rao`, when the coldkey free balance cannot pay the burn, or when the estimated fee exceeds `--fee_limit_rao`. A hotkey already registered under the same coldkey reports its UID and stops.

> [!IMPORTANT]
> The default run broadcasts nothing. `--apply` is the only way to submit, and it waits for canonical finality before reporting the UID:

```bash
provider fleet register --manifest=/path/to/fleet-manifest.json --hotkey_seed_file=/path/to/hotkey.seed --coldkey_seed_file=/path/to/coldkey.seed --substrate=<ws_url> --apply
```

The broadcast is journaled under the provider state (exact SCALE bytes plus a transaction log), and a retained write replays only with `--apply`, never as a side effect of an ordinary run.

## publish

`publish` writes the manifest commitment hash on-chain, signed by the fleet hotkey through `--substrate`, and then verifies the commitment at the finalized block before printing success:

```bash
provider fleet publish --manifest=/path/to/fleet-manifest.json --hotkey_seed_file=/path/to/hotkey.seed --substrate=<ws_url>
```

There is no plan flag and no dry run for publish: it always writes. Before signing, it refuses a hotkey that is the subnet owner hotkey or whose UID holds a validator permit; the role is read at the finalized block it signs against and any read error refuses. Run it after `register`, and again whenever the manifest generation changes. Commitments are written in the tagged `urfleet1` (Raw40) format that the commitment oracle mirrors, so a fleet published before the tag holds an untagged commitment and must be republished with `provider fleet publish` before ~block 9,360,673.

## bind

`bind` is the dual-signed member binding: the client Ed25519 key and the fleet hotkey sr25519 key both sign the same digest, and the transaction is relayed by `--relayer_key_file`, which pays gas and receives no binding ownership. `--valid_from_epoch` and `--valid_to_epoch` set the window:

```bash
provider fleet bind \
  --manifest=/path/to/fleet-manifest.json \
  --client_id=<client_id_hex> \
  --client_seed_file=/path/to/client.seed \
  --hotkey_seed_file=/path/to/hotkey.seed \
  --valid_from_epoch=<first_epoch> \
  --valid_to_epoch=<last_epoch> \
  --rpc=<evm_rpc_url> \
  --relayer_key_file=/path/to/relayer.key
```

Before anything is sent, the command walks `--rpc` endpoints in order, checks the reported chain id against the manifest `chain_id`, simulates the call and estimates gas. With `--dry-run` it stops there and prints the intent without sending; without it, the transaction is sent and waited on through mining and finality.

> [!WARNING]
> On mainnet a bind reverts with `StaleCommitment()` until the commitment oracle has copied your published commitment to chain 964. As of sn `83e6f48c2` the oracle is built but not deployed. See [Head Tier: current blockers](Bittensor-Head-Tier.md#current-blockers).

## sign

`sign` builds exactly the same binding as `bind` and signs it locally, for the operator who submits through the Top 200 submission page instead of from this host:

```bash
provider fleet sign --manifest=/path/to/fleet-manifest.json --client_id=<client_id_hex> --client_seed_file=/path/to/client.seed --hotkey_seed_file=/path/to/hotkey.seed --valid_from_epoch=<first_epoch> --valid_to_epoch=<last_epoch>
```

With `--client_seed_file` it prints one line of payload JSON (the `binding`, the `client_signature`, and the `hotkey_signature` when the hotkey seed is also given) for the page; with only `--hotkey_seed_file` it prints the bare hotkey signature. The digest and labels go to stderr. `--client_id` is optional only when the client seed is given: without it, the command selects the one manifest member whose `client_key` matches the seed. There is no RPC, no relayer key and no network access, and seeds are never printed or written, including in errors.

## status

`status` reads the commitment back through `--substrate` and the coordinator record through `--rpc` for one `--client_id`, then compares both against the canonical manifest:

```bash
provider fleet status --manifest=/path/to/fleet-manifest.json --client_id=<client_id_hex> --substrate=<ws_url> --rpc=<evm_rpc_url>
```

It prints the manifest commitment, the native commitment with its block and finality, the coordinator address, and the client's generation, epoch window, cleaned counter and UID. A mismatch exits 1 with `fleet status does not match the canonical manifest`, which is the fastest way to tell whether a publish or a binding landed as intended.

## revoke

`revoke` ends one member's binding at a future epoch. The command reads the revocation digest from the coordinator, checks it against the locally built signing domain, signs with the member's `--client_seed_file`, and relays through `--relayer_key_file`:

```bash
provider fleet revoke --manifest=/path/to/fleet-manifest.json --client_id=<client_id_hex> --client_seed_file=/path/to/client.seed --effective_epoch=<future_epoch> --rpc=<evm_rpc_url> --relayer_key_file=/path/to/relayer.key
```

Without `--dry-run` the transaction is sent and waited on; `--dry-run` stops at the same preflight as `bind` and sends nothing.

## The runtime authority pin

Fleet commands authenticate the native runtime before they sign or decode anything. What they authenticate depends on the manifest chain.

- Mainnet (chain 964): `--mainnet-runtime-authority=<path>` plus `--mainnet-runtime-authority-sha256=<64 lowercase hex digits>` pin the independently reviewed runtime authority document by its exact bytes. The document is strict JSON (schema `urnetwork-mainnet-fleet-runtime-authority-v1`, or the `-v2` catalog form listing up to 8 reviewed artifacts with their allowed purposes; at most 16 KiB) that binds the chain, subnet and coordinator to the reviewed runtime version, code hash and metadata hash, and it must agree with the manifest. Every endpoint that becomes the signing connection must authenticate the runtime against that pin, including the fresh genesis and chain name; endpoints are tried in order and a non-matching one is refused before any signing. On the EVM side, failover is limited to failed transport construction, so a reached wrong chain id or artifact is a hard refusal rather than approval by fallback.
- Testnet (chain 945): the release runtime artifact is the default. `--provisional-runtime-compatibility=<profile>` with `--runtime-observation-dir=<path>` selects the explicit consumed-runtime profile (`urnetwork-subtensor-consumed-interface-v1`), with an absolute observation directory that receives the observations. The flags are presence-paired: giving one without a valid value for the other is refused.

> [!WARNING]
> Mainnet writes are refused without both mainnet authority flags and matching bytes, and the provisional testnet flags cannot be combined with them. No production genesis, source or runtime pin is shipped: the operator supplies the reviewed document.

## Durable volumes for fleet writes

Mainnet fleet writes open their journals through an owner-local storage declaration, not through the filesystem under the state directory alone. `--durable-volumes=<path>` names the declaration file and `--durable-volumes-sha256=sha256:<64 lowercase hex digits>` pins its exact bytes:

```bash
provider fleet register --manifest=/path/to/fleet-manifest.json --hotkey_seed_file=/path/to/hotkey.seed --coldkey_seed_file=/path/to/coldkey.seed --substrate=<ws_url> --durable-volumes=/path/to/owner-local-volumes.json --durable-volumes-sha256=sha256:<declaration_sha256>
```

The declaration uses the owner-local schema `urnetwork-owner-local-volumes-v2` and lists the pre-provisioned state roots the journals may live under. The CLI never creates, enrolls or infers storage, and both flags must be given together. Without them a mainnet write fails early with `explicit durable-volume declaration and hash are required`.

> [!NOTE]
> This is a different schema from the claim daemons' declaration (`urnetwork-durable-volumes-v2`, see [Claim Daemons](Bittensor-Claim-Daemons.md#durable-volumes)). The daemons refuse an owner-local file, so you cannot reuse one declaration for both.

## Seed files

Three flags read seed material, always by path: `--hotkey_seed_file` (sr25519 fleet hotkey), `--coldkey_seed_file` (sr25519 fleet coldkey) and `--client_seed_file` (Ed25519 member client key).

- Content is 32 raw bytes, or 64 hex characters with an optional `0x` prefix. The sr25519 seed files must additionally be single-link regular files owned by the effective user, mode `0600` or read-only `0400`, at most 4096 bytes, under a parent directory that is not group or other writable.
- The CLI never creates a seed file: a missing path is an error, and there is no generated fallback.
- Seeds are never printed, logged or written anywhere by these commands, including in error messages.
- Keep the fleet coldkey seed off mining hosts. The hotkey and client seeds are fleet-critical too: anyone who reads them can sign bindings for the fleet.

## Recovery

Mainnet fleet writes are crash-safe by retaining signed liabilities, not by trusting a single broadcast. The custody behavior is in `provider/fleet_recovery_store.go`, `provider/fleet_recovery_native.go` and `provider/fleet_recovery_evm.go`.

- The journal lives under the provider state in `fleet-mainnet-recovery/` (`journal.json` plus an initialized marker), is private to the owner (`0700`), and takes an exclusive lock, so a second fleet writer is refused instead of being interleaved. Register additionally journals under `fleet-native/` (exact SCALE bytes plus `native-transactions.jsonl`).
- Every record retains the exact intent (action, canonical manifest bytes, client id, epochs, genesis), the original reviewed authority bytes and their sha256, the signer, the nonce, the exact signed transaction and a custody signature over the record. The journal is signed as a whole, so a removed record cannot pass validation, and a finalized record can never be replaced.
- Stages are `prepared`, `may_have_sent` and `finalized`. While a record is unresolved, a different intent is blocked; a retained native write is replayed only with `--apply`, and an EVM replay is disabled by `--dry-run`.
- Re-running the same command is the supported recovery path. On restart the store reconciles an uncertain publication from its retained bytes; the command then finds the retained record and either completes it or proves what happened. A native write scans the finalized archive in bounded chunks for the retained extrinsic and only retransmits the exact original bytes if it truly never landed and the original nonce is still available. An EVM write checks the receipt for the retained transaction hash first; if the transaction landed, it completes finality, exact-bytes inclusion, event and contract readback checks. A finalized success reports `already finalized`; a finalized failure is reported on every retry and never silently replaced.
- Unsupported platforms refuse the mainnet write path instead of falling back to weaker custody.

## bind-head and unbind-head

`provider bind-head` and `provider unbind-head` are unchanged in this release: not removed, not renamed, kept beside the fleet group. They are the pre-fleet EVM-only path with their own grammar and `--key_file` flow; use the `provider fleet` group for new work. Their grammar is in [Bittensor operations](Bittensor-Operations.md#bind-head-and-unbind-head-old-head-tier-path).

## What to check after

- `provider fleet manifest` before anything else: the printed commitment sha256 is the value that must appear in `fleet status` and in the publish output.
- `provider fleet register --apply`: the final UID line, and the same UID in `fleet status`.
- `provider fleet publish`: `fleet commitment finalized` with the commitment hash, extrinsic hash and finalized block.
- `provider fleet bind` or `provider fleet revoke`: the finalized transaction line and the matching coordinator event, then `fleet status` for the epoch window.
- On disk: the provider state directory holds `fleet-mainnet-recovery/` (mainnet custody) and `fleet-native/` (native transaction journal); both must stay private. The default provider state is `~/.urnetwork`, overridable with `URNETWORK_STATE_DIR`.
