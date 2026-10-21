# Bittensor Hotkey Delegation

> [!TIP]
> **Most operators can stop here.** With one listed operator, a website account and a signed network consent (see the [Quick Start](Bittensor-Quick-Start.md)) you do not need hotkey delegation. An account created by `--auto-register --hotkey_seed_file` is a separate URnetwork account that website consent does not cover ([Multi-Operator](Multi-Operator.md)), so those operators still need delegation or a network consent for that account. This page is for people who sign in with a hotkey across several operators.


One coldkey signature pays the miner on every operator. This page covers the global hotkey wallet consent and the per-operator delegation in detail. [Bittensor operations](Bittensor-Operations.md) covers the wider Subnet 25 surface, and [Multi-Operator](Multi-Operator.md) covers running one provider per operator in the published list.

## What the delegation is

The hotkey wallet consent is a global statement, kept locally as a chain of generations. The coldkey and the hotkey each sign every generation once. A later generation becomes the head of the chain, and it can name a different coldkey; the subnet and the hotkey do not change from generation to generation.

The operator stores the chain, and the hotkey alone signs a delegation of the miner's network to the chain's head there. Because the delegation is signed by the hotkey, the coldkey never signs per operator.

At an operator the order of payment is fixed: a provider's own signed consent is paid first, then a signed network consent, and the hotkey delegation's coldkey only after both. The status output echoes that order in its caveats.

The chain lives in the provider state directory (`~/.urnetwork`): `hotkey-wallet/originals.json` holds the signed generations, and `hotkey-wallet/pending.json` holds the statement that awaits its signatures.

## When to use it

There is one operator in the published list today, and a signed network consent already covers you there, so most people do not need this. Use it when the miner's network should earn to one coldkey across the operators it runs on, without signing a wallet consent at each operator. One coldkey signature, made once, covers every operator that stores the chain.

Timing: a delegation starts two epochs after it is signed and is not retroactive. The sn module pinned by this tree (its miner wallet command, the generation this provider ports) states the rule for a delegation: it earns from the operator's current epoch plus 2, past the operator's prospective boundary. The provider builds the delegation window the same way, so sign early if an epoch matters; epochs already under way are not covered.

To change the earning coldkey later, run the flow again: the new generation becomes the head, and each operator's delegation is moved to it.

## The two-step flow

`provider wallet hotkey challenge` builds the next consent statement, keeps it pending in `hotkey-wallet/pending.json`, and prints the exact message for the coldkey to sign. The statement names the next generation, the subnet, the hotkey and the coldkey.

The printed guidance says the bytes to sign are the UTF-8 text between the message markers exactly as printed (LF line endings, no trailing newline), signed with sr25519 in the "substrate" signing context; a Polkadot extension signRaw of type "bytes", which wraps the message in `<Bytes>...</Bytes>`, is accepted too. It ends with the set command to run with the signature.

`provider wallet hotkey set` completes the statement in one of two ways. With `--coldkey_seed_file` the coldkey signs locally, and the file must derive the `<coldkey_ss58>` given. With `--message` and `--signature`, the command verifies them against the pending statement and refuses anything that is not the message challenge printed last.

Set then adds the hotkey signature, verifies the complete chain, appends the generation to `originals.json`, and submits the chain and the delegation to the operator this provider resolves (its saved network config, otherwise the default platform URL). The operator is reported on its own line; if the submission failed, the command exits 1 and says: `N of M authenticated operators do not adopt the head yet; set it again with the same --message and --signature`. Run a set from each provider you run, so every operator you serve stores the chain and delegation.

Setting the same message again resends. If the generation is already stored, set reuses the stored head and resubmits the chain and the delegation; with `--coldkey_seed_file` and the same coldkey and epochs it prints `generation N is already stored for this coldkey and these epochs; resubmitting it` instead of signing a new generation. This also renews a delegation whose window has passed: the resubmission signs a fresh delegation window at the operator.

A set whose submission failed has already stored the generation locally: the chain file is written before the operator is asked, and a retry resubmits that head instead of signing a new generation.

The two steps as commands, with the coldkey signature made elsewhere:

```bash
# build the statement and print the message for the coldkey
provider wallet hotkey challenge 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5 --hotkey_seed_file=/secure/hotkey.seed

# submit the signature; the challenge prints this command with the message filled in
provider wallet hotkey set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5 --hotkey_seed_file=/secure/hotkey.seed \
    --message='<the one-line message from the challenge>' --signature=0x<128 hex chars>
```

With the coldkey seed on the same host, one command is enough on its own: `provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path> --coldkey_seed_file=<path>` builds the pending statement when none matches and signs it locally.

## Command grammar

```text
provider wallet hotkey challenge <coldkey_ss58> --hotkey_seed_file=<path>
    [--wallet-from-epoch=<epoch>] [--wallet-through-epoch=<epoch>] [--operators-url=<url>]
    [-v...]
provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path>
    [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]
    [--wallet-from-epoch=<epoch>] [--wallet-through-epoch=<epoch>] [--operators-url=<url>]
    [-v...]
provider wallet hotkey status [--hotkey_seed_file=<path>] [--operators-url=<url>] [-v...]
```

- `--hotkey_seed_file` is required by challenge and set; status reads it only to check the seed against the chain's hotkey and warns when it is not that key.
- `--coldkey_seed_file` is the local coldkey signature for `set`. The file must be mode 600 and is never created.
- `--message` and `--signature` are the offline signature: the exact message challenge printed, or its one-line form with a literal \n per line break, and the coldkey's 64-byte sr25519 signature as hex (0x optional). They go together.
- `--wallet-from-epoch` and `--wallet-through-epoch` select the consent's earning window and go together. The window holds at most 65536 epochs: the through epoch is at most 65535 after the from epoch.
- `--operators-url` names the operator list used by the operators family ([Multi-Operator](Multi-Operator.md)); the hotkey lane resolves its one operator from the provider's saved network config, not from the list.

## Generations and earning windows

Generation 1 starts the chain. Every later generation numbers one higher and links the previous generation's hash. A new generation must move forward: its from epoch must be later than the previous generation's from epoch, and its subnet and hotkey must match the chain.

Generation 1 takes its subnet (chain, genesis hash and netuid) from the authenticated operator, which states it; later generations keep it. Without epoch flags, generation 1 is built for epochs 0 through 65535, and a later generation starts at the operator's current epoch plus 1, or just after the previous generation's start when that is later.

Every consent window ends 65535 epochs after its start. If you pass the epoch flags, the same bound applies.

## Status output

`provider wallet hotkey status` prints the local chain first: one line per generation (`generation N: coldkey ..., epochs A through B, original 0x...`), then the head (`head: generation N, 0x..., hotkey ..., chain C netuid D`), followed by the pending statement while one is waiting. When `--hotkey_seed_file` names a key that is not the chain's hotkey, the head line is followed by `warning: --hotkey_seed_file is not the chain's hotkey`.

Then it prints the delegation the operator holds:

- `adopts the head, generation N, from epoch A through B` when the operator's delegation pins the chain head, with the earning window shown.
- Caveats after that line: `not effective: the window ends at epoch A and the operator is at epoch B; the next provide upkeep renews it` when the window ends before the operator's current epoch plus 2 (a running provider renews it on its hourly upkeep, and setting the same message and signature again renews it too); a signed network wallet consent on the operator (with its coldkey) outranks the delegation, so that coldkey is paid; and a per-provider signed consent outranks both for that provider (the command notes it is not listed).
- `delegates to hotkey X at generation N, not the head` when the operator holds a delegation to another head or another hotkey.
- `no hotkey delegation` when the operator holds none, and `unavailable: <error>` when it cannot be asked.

## Safety

The signer is deliberately not general-purpose. It signs only a hotkey wallet consent or a hotkey delegation; anything else fails with `refusing to sign a message that is neither a hotkey wallet consent nor a hotkey delegation`.

A delegation challenge is signed only after the protocol decoder accepts it and it names exactly the requested hotkey, consent head, epochs and network, so a hostile operator cannot obtain a signature that delegates another network or approves anything else.

Seed files are read through the sn seed custody: a private file the caller owns, never created and never logged. The `--coldkey_seed_file` help says to use it on a workstation, not on a fleet node.

A missing or unsafe seed file is an error that names its path; the command never creates a key in its place.

By design the seed-bearing commands do not run on Windows: the seed custody supports Linux and macOS only, and on Windows a seed load fails with `private seed file custody is unsupported on this platform`. Run them on a Linux or macOS host; status without `--hotkey_seed_file` reads no seed.

## What to check after

Run `provider wallet hotkey status` and read the operator line: the operator should `adopts the head` with an earning window that extends past its current epoch. A window flagged `not effective` ends before the operator's epoch plus 2; the next provide upkeep renews it, and setting the same message and signature again renews it too.

When a set exits 1 over operator failures, its output names them; run the same set again. The retry stores the chain and renews a delegation where the head is missing or its window no longer clears the two-epoch rule; an operator that already adopts the head reports `chain stored; the delegation already adopts generation N`.

On a provider that runs one child per operator, `provider operators` shows the list and each operator's credential state; see [Multi-Operator](Multi-Operator.md).

The set output names what was stored, for example `hotkey wallet generation N: coldkey ..., epochs A through B, head 0x...`. Keep the local chain (`hotkey-wallet/originals.json` and `hotkey-wallet/pending.json`) backed up, and keep the seed files private.

```bash
provider wallet hotkey status
```
