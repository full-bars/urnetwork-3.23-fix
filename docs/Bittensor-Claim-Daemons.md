# Bittensor Claim Daemons

> [!TIP]
> **Most operators can stop here.** Claims are optional. Under settlement v2 anyone can send a claim, credit never expires, and you can claim whenever the balance is worth the gas. Daemons are only for people who want this automated. See the [Quick Start](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start).

> [!NOTE]
> Upstream has set settlement v2 to start at epoch 3 (block 9,348,028, about 2026-10-23 18:10 UTC), as of sn `83e6f48c2`. It still depends on the release checks passing. Earlier epochs are settlement v1, and v1 claims stay open for 8 epochs plus 1 epoch of grace, so keep the v1 daemon (or `--v1-config`) running until those are claimed.


`provider claim-daemon` (v1) submits a pool's claims as epochs finalize. `provider claim-daemon-v2` (v2) works differently: it polls finalized account roots and claims once an account reaches the configured threshold, so it does not place one claim per epoch. Either one removes the need to run `provider claim` by hand, and claiming stays optional and not urgent. Each daemon reads one strict YAML config file, drives its own claim queue under the config's state directory, and reconciles already signed transactions after a restart. For the wider subnet workflow, see [Bittensor operations](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Operations).

## What the daemons do

Both daemons poll on the config's `poll_seconds` cadence, discover what the API and the chain can support, and submit with one EVM relayer key over the configured RPC endpoints. Both run until stopped (SIGINT, SIGQUIT or SIGTERM), and both require an explicit durable-volume declaration, described below.

v1 keeps one queue entry per claimable epoch. It reads the current epoch, discovers the claimable epochs in the configured lookback, fetches each epoch's claim artifact with the retained claim credential, verifies the payout root on chain at a finalized block, and submits the claim through the relayer key. The epoch is recorded finalized only from canonical on-chain evidence (see Finality below).

v2 keeps one unit per `(vault, no_id, account index)` for every configured coldkey. It reads the finalized accounts from the public ledger endpoint, verifies each account's Merkle proof against the vault root at a finalized block, and claims once the claimable balance reaches the threshold. A v2 unit is not terminal: an idle account reopens when a newer finalized root raises it above its claimed watermark.

## Requirements and when to use which

Both daemons need:

- A strict YAML config file (`--config=<path>`): unknown keys are refused and the file must be exactly one YAML document.
- An EVM relayer private key file (`key_file`) that must already exist; the daemon never creates it.
- A private `state_dir` owned by the daemon user on a declared durable volume.
- The durable-volume declaration: `--durable-volumes=<path>` with `--durable-volumes-sha256=<hash>`.
- One or more EVM JSON-RPC endpoints (`rpc`): they are tried in order until one serves the configured chain.

| Daemon | Settlement | Extra inputs | Credential |
|---|---|---|---|
| v1 (`claim-daemon`) | settlement v1 epoch claims | optional `legacy_coldkey`, `jwt_file`, `lookback_epochs`, `progress_pool` | a retained claim credential (`jwt_file` or the provider's own token) |
| v2 (`claim-daemon-v2`) | settlement v2 vault accounts | `chain_id`, `vault`, `coldkeys`, optional `batcher` | none: it reads the public ledger endpoint and signs with the relayer key |

Run v1 when the operator's claimable payouts are settlement v1 epoch claims. Run v2 for settlement v2 accounts. `provider claim-daemon-v2 --v1-config=<path>` runs the unchanged v1 daemon beside v2 in one process, so both claim families are covered during the transition.

## Quick start

v2, with a config adapted from [the annotated example config](/full-bars/urnetwork-3.23-fix/blob/main/provider/testdata/claim-daemon-v2.example.yml):

```yaml
schema_version: 2
api_url: "https://api.bringyour.com"
rpc:
  - "<evm-rpc-url>"
chain_id: 964
vault: "<vault2-address>"
batcher: "<claim-batcher-address>"
coldkeys:
  - "<coldkey-ss58>"
key_file: "<relayer-key-file>"
state_dir: "<state-dir>"
poll_seconds: 30
```

Replace every `<...>` placeholder; `batcher` may be removed entirely (it is optional). Then run:

```bash
provider claim-daemon-v2 --config=/etc/urnetwork/claim-daemon-v2.yml \
  --v1-config=/etc/urnetwork/claim-daemon-v1.yml \
  --durable-volumes=/etc/urnetwork/durable-volumes.json \
  --durable-volumes-sha256=sha256:<64 lowercase hex digits>
```

Drop `--v1-config` when only settlement v2 needs claiming. The v1 daemon:

```yaml
schema_version: 1
release: "1.0"
api_url: "https://api.bringyour.com"
rpc:
  - "<evm-rpc-url>"
key_file: "<relayer-key-file>"
state_dir: "<state-dir>"
jwt_file: "<client-jwt-file>"
poll_seconds: 30
lookback_epochs: 2
```

```bash
provider claim-daemon --config=/etc/urnetwork/claim-daemon-v1.yml \
  --durable-volumes=/etc/urnetwork/durable-volumes.json \
  --durable-volumes-sha256=sha256:<64 lowercase hex digits>
```

Omit `jwt_file` to use the retained provider token instead. The full grammar:

```text
provider claim-daemon --config=<path>
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
provider claim-daemon-v2 --config=<path> [--v1-config=<path>] [--threshold-rao=<rao>] [--claim-now]
    [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
    [-v...]
```

## v1 configuration reference

One strict YAML document; unknown keys are refused. Relative `key_file`, `state_dir` and `jwt_file` are anchored at the config file.

| Key | Required | Default | Rules |
|---|---|---|---|
| `schema_version` | yes | none | must be `1` |
| `release` | yes | none | must be `"1.0"` |
| `api_url` | yes | none | API base URL for epoch discovery and claim artifacts |
| `rpc` | yes | none | non-empty list of EVM JSON-RPC endpoints, tried in order |
| `key_file` | yes | none | relayer EVM private key file; must exist when the config loads |
| `legacy_coldkey` | no | none | ss58 coldkey (prefix 42) selecting the legacy network-token credential path |
| `jwt_file` | no | none | client token file; when present on disk it must be a private regular file (no group or other permissions) |
| `state_dir` | yes | none | canonical absolute path, private and owned; see the state directory section |
| `poll_seconds` | no | `30` | 5 to 3600 |
| `lookback_epochs` | no | `2` | at most 256; the first run starts discovery this many epochs back from the last finalized epoch |
| `progress_pool` | no | none | declared claim pool (`chain_id`, `vault`, `no_id`, `coldkey`) the progress census is compared against; validated when present |

A missing or malformed value in any required key refuses the whole file (`invalid release-1.0 claim daemon configuration`), and a `key_file` that does not exist refuses at load (`claim relayer key: ...`).

## v2 configuration reference

One strict YAML document; unknown keys are refused. Relative `key_file` and `state_dir` are anchored at the config file.

| Key | Required | Default | Rules |
|---|---|---|---|
| `schema_version` | yes | none | must be `2` |
| `api_url` | yes | none | API base URL for the public ledger account read |
| `rpc` | yes | none | non-empty list of EVM JSON-RPC endpoints, tried in order until one serves `chain_id` |
| `chain_id` | yes | none | EVM chain id, non-zero |
| `vault` | yes | none | settlement vault, lowercase `0x` hex address; pinned in the config |
| `batcher` | no | none | claim batcher, lowercase `0x` hex, must differ from `vault`; verified at startup |
| `coldkeys` | yes | none | non-empty list of ss58 coldkeys (prefix 42) |
| `key_file` | yes | none | relayer EVM private key file; must exist when the config loads |
| `state_dir` | yes | none | canonical absolute path, private and owned |
| `poll_seconds` | no | `30` | 5 to 3600 |

The vault is never taken from the operator's endpoint: accounts the endpoint reports under another vault are skipped, and every signed claim names the configured `vault` (or the configured `batcher`). A configured batcher must be the release claim batcher; the daemon verifies it once at startup and refuses to start otherwise.

## Credential and JWT freshness (v1)

`provider claim-daemon` never mints or refreshes a credential. It reads the retained claim credential: `jwt_file` when the config sets one, otherwise the retained provider token in the provider state directory (`.provider.jwt`, or `jwt` in the explicit `legacy_coldkey` network mode).

While that credential is missing, empty or no longer accepted, the daemon prints one line to stderr (`claim-daemon: <reason>; driving retained signed work only`) and stops taking new work, while still driving entries whose exact bytes were already signed. At startup, when there is no retained signed entry, it exits 1 with the credential error instead. A valid credential that appears later resumes new work without a restart.

> [!NOTE]
> `provider claim-daemon-v2` needs no operator credential of its own. With `--v1-config` the v1 daemon beside it keeps the credential rules above.

## Threshold, `--claim-now` and `--v1-config`

- `--threshold-rao=<rao>` claims an account once its claimable balance reaches this many rao. The default is 1,900,000,000 rao (1.9 alpha); a value below the 15,200,000 rao (0.0152 alpha) transfer floor is refused at startup.
- `--claim-now` claims every account with anything claimable, once, at start; after that first pass the threshold rules apply again.
- `--v1-config=<path>` runs the unchanged v1 daemon of that configuration in the same process. Both daemons must share one relayer key (the same parsed `key_file`; a mismatch is refused) and they share its nonce admission; each `state_dir` has one owner, so the two configs need different `state_dir` values.

## Durable volumes

Both daemons refuse to start without the explicit declaration: `--durable-volumes=<path>` and `--durable-volumes-sha256=<hash>` (format `sha256:<64 lowercase hex digits>`) must both be given, and the file's SHA-256 must match the declared hash. The declaration is JSON with the daemon-volume schema `urnetwork-durable-volumes-v2`: it names each mount, its filesystem identity, a marker, and one or more precreated state roots, each pinned by inode and generation hash.

Parsing never enrolls storage, infers a mount, or chooses a synthetic host. The declaration file itself must be a protected regular file (owned by root or the daemon user, not group or other writable, single link), and the config's `state_dir` must be a declared state root or live beneath one.

> [!WARNING]
> The flags are bracketed in the grammar, but at runtime both flags and a matching declaration are required. The owner-local schema (`urnetwork-owner-local-volumes-v2`) is a different scope and is refused by the daemons.

## State directory and restart behavior

`state_dir` must be an absolute, already clean path (no `..` or trailing slash); symlinked components are resolved to the physical directory. The daemon opens the directory with an exclusive lock, so it must already exist, be private (no group or other permissions) and be owned by the daemon user. A second daemon pointed at the same directory is refused, and a new owner refuses to read an existing queue entry that is not a private owned regular file.

Progress is journaled under `state_dir` as the queue file (`claim-queue.json`). Every signed transaction is written with its exact raw bytes before broadcast, so already signed entries survive a restart: the next owner reconciles those bytes against the chain and never replaces them with a new transaction. An entry that was submitting when the process stopped is recovered as uncertain when signed bytes exist, or retried when they do not. Reconciling a signed entry needs no credential at all: only the RPC endpoints and the relayer key.

## Finality

In v1, an epoch counts as claimable only after the daemon reads the settlement vault at the RPC endpoint's finalized block and the epoch's on-chain payout root equals the root in the claim artifact. A mismatch is refused and retried, never signed. If the leaf is already claimed at that block, the epoch is recorded finalized. A submitted claim becomes finalized only from a canonical receipt at a finalized block.

In v2, the daemon reads each account's vault root at a finalized block and verifies the account's Merkle proof against it before anything is signed. A proof that does not verify is retried. A v2 unit settles from its finalized receipt (the vault's Claimed event), and a claim refused at preflight or reverted in a block rechecks the account's claimed watermark before retrying.

## What to check after starting

- Startup failures exit 1 with a `claim-daemon:` or `claim-daemon-v2:` prefix naming the failing check (config, durable declaration, key file, batcher, threshold).
- v1 with retained signed work and no usable credential prints exactly one stderr line: `claim-daemon: <reason>; driving retained signed work only`. New work stays paused until the credential works again; with nothing retained signed the daemon exits 1 instead.
- The queue file under `state_dir` shows each entry's status. v1 statuses: `pending`, `retry`, `submitting`, `uncertain`, `finalized`, `no-claim`; v2 statuses: `idle`, `open`, `submitting`, `uncertain`.
- A configured v2 batcher is verified at startup, and a second daemon on the same `state_dir` is refused as already owned.
- `-v...` enables verbose mode (`-v` level 1, `-vv` level 2, and so on).
