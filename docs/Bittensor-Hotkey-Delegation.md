# Bittensor Hotkey Delegation

> [!TIP]
> **Most operators can stop here.** If you log in with a website account and have signed the network consent (see the [Quick Start](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start)), you do not need hotkey delegation. There is one operator in the published list today, and your network consent already covers you there.

> [!NOTE]
> Checked against upstream sn `83e6f48c2` (2026-10-21). This build pins sn `6608298ead6b`, and the hotkey wallet code (`hotkeywallet/`, `miner/hotkey_wallet_cmd.go`, `protocol/earning_wallet.go`) is unchanged between the two.

## Do I need this?

| Your situation | Do you need it? |
|---|---|
| You log in with a website account and signed the network consent | **No.** |
| You sign in with `--auto-register --hotkey_seed_file`, on one operator | Maybe. That creates a **separate** URnetwork account owned by the hotkey, and your website consent does not cover it ([Multi-Operator](/full-bars/urnetwork-3.23-fix/wiki/Multi-Operator)). That account needs either a network consent of its own or this delegation. |
| You run the same hotkey on several operators and want one coldkey paid everywhere | **Yes.** This is what the delegation is for. |

## In plain words

You sign **one** statement with your coldkey that says "this hotkey earns to this coldkey". Your hotkey then tells each operator to pay that coldkey. The coldkey never has to sign anything per operator.

Two pieces, two signers:

- **The hotkey wallet consent**: one statement, signed by both the coldkey and the hotkey. It is kept on your computer as a chain of generations. A new generation can switch to a different coldkey.
- **The delegation**: signed by the hotkey alone, stored at each operator. It points that operator at the newest generation (the "head") of your consent chain.

## Which wallet gets paid

At an operator the order is fixed. For each node and epoch, the first one that applies wins:

1. A per-provider consent for that node.
2. A network consent on the account.
3. The hotkey delegation's coldkey.

So the delegation is a fallback. **If the hotkey's account already has a network consent in effect, the delegation pays nothing**, and `hotkey status` says so in its caveats. The delegation is only used when the account has no network consent, or none in effect that epoch. A per-provider consent that is broken (bad signature, missing history) does not fall back to anything: that node's wallet stays unresolved for the epoch.

## When it starts

A delegation earns from the operator's current epoch **plus 2**. It is never backdated. Sign early if an epoch matters to you, because epochs already under way are not covered.

## Quick recipe

Run this on your own Linux or macOS computer, with the hotkey seed file there. Run the `set` step once from **each provider you run**, so every operator you serve stores the chain and the delegation.

**Coldkey signs somewhere else** (extension, btcli, hardware wallet):

```bash
# 1. build the statement and print the message for the coldkey to sign
provider wallet hotkey challenge 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5 --hotkey_seed_file=/secure/hotkey.seed

# 2. submit the signature; the challenge prints this exact command with the message filled in
provider wallet hotkey set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5 --hotkey_seed_file=/secure/hotkey.seed \
    --message='<the one-line message from the challenge>' --signature=0x<128 hex chars>

# 3. check
provider wallet hotkey status
```

**Coldkey seed on the same computer**: one command builds the statement and signs it locally:

```bash
provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path> --coldkey_seed_file=<path>
```

Then read [What to check after](#what-to-check-after).

**To change the coldkey later**, run the recipe again with the new coldkey. The new generation becomes the head, and each operator's delegation moves to it.

## How the two steps work

`provider wallet hotkey challenge` builds the next consent statement, keeps it pending in `hotkey-wallet/pending.json`, and prints the exact message for the coldkey to sign. The statement names the next generation, the subnet, the hotkey and the coldkey.

What to sign: the UTF-8 text between the message markers, exactly as printed (LF line endings, no trailing newline), with sr25519 in the "substrate" signing context. A Polkadot extension `signRaw` of type "bytes", which wraps the message in `<Bytes>...</Bytes>`, is accepted too. The output ends with the `set` command to run.

`provider wallet hotkey set` completes the statement in one of two ways:

- With `--coldkey_seed_file`, the coldkey signs locally. The file must derive the `<coldkey_ss58>` you gave.
- With `--message` and `--signature`, the command checks them against the pending statement and refuses anything that is not the last printed challenge.

Then `set`:

1. adds the hotkey signature and verifies the whole chain,
2. writes the generation to `hotkey-wallet/originals.json`, **before** asking the operator,
3. submits the chain and the delegation to the operator this provider uses (its saved network config, otherwise the default platform URL).

The operator gets its own output line. If submission fails, the command exits 1 with `N of M authenticated operators do not adopt the head yet; set it again with the same --message and --signature`. Because the generation is already stored locally, a retry resubmits that head instead of signing a new one.

Running the same `set` again is always safe:

- It reuses the stored head and resubmits the chain and the delegation.
- With `--coldkey_seed_file` and the same coldkey and epochs it prints `generation N is already stored for this coldkey and these epochs; resubmitting it` instead of signing a new generation.
- It also renews a delegation whose window has passed, because the resubmission signs a fresh delegation window at the operator.

The chain lives in the provider state directory (`~/.urnetwork`, or `URNETWORK_STATE_DIR`): `hotkey-wallet/originals.json` holds the signed generations and `hotkey-wallet/pending.json` holds a statement waiting for its signatures. **Back both up.**

## What to check after

Run `provider wallet hotkey status` and read the operator line.

| You see | Meaning | What to do |
|---|---|---|
| `adopts the head, generation N, from epoch A through B` and no caveats | Working. | Nothing. |
| `not effective: the window ends at epoch A and the operator is at epoch B; ...` | The delegation window ends before the operator's epoch plus 2. | A running provider renews it on its hourly upkeep. Or run the same `set` again. |
| A caveat naming a signed **network** consent | That consent outranks the delegation, so its coldkey is paid. | Nothing, if that is the coldkey you want. Otherwise replace the network consent (see the [Quick Start](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start#changing-your-wallet)). |
| `delegates to hotkey X at generation N, not the head` | The operator holds an older head or another hotkey. | Run `set` from that provider. |
| `no hotkey delegation` | The operator holds none. | Run `set` from that provider. |
| `unavailable: <error>` | The operator could not be asked. | Check the network, then retry. |
| `warning: --hotkey_seed_file is not the chain's hotkey` | The seed file you passed is a different key. | Use the hotkey the chain was built with. |

A per-provider consent outranks both, but `status` does not list those. It only notes that they exist.

If a `set` exited 1 over operator failures, its output names them. Run the same `set` again. An operator that already has the head reports `chain stored; the delegation already adopts generation N`.

On a provider that runs one child per operator, `provider operators` shows the list and each operator's credential state. See [Multi-Operator](/full-bars/urnetwork-3.23-fix/wiki/Multi-Operator).

## Command reference

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

| Flag | Meaning |
|---|---|
| `--hotkey_seed_file` | Required by `challenge` and `set`. `status` reads it only to check it against the chain's hotkey. |
| `--coldkey_seed_file` | Local coldkey signing for `set`. Mode `0600`, never created by the tool. Use it on your own computer, never on a fleet node. |
| `--message`, `--signature` | The offline signature, always together. The message is exactly what `challenge` printed, or its one-line form with a literal `\n` per line break. The signature is the coldkey's 64-byte sr25519 signature as hex (`0x` optional). |
| `--wallet-from-epoch`, `--wallet-through-epoch` | The consent's earning window, always together. At most 65,536 epochs: through is at most 65,535 after from. |
| `--operators-url` | The operator list used by the operators family ([Multi-Operator](/full-bars/urnetwork-3.23-fix/wiki/Multi-Operator)). The hotkey commands still resolve their one operator from the provider's saved network config, not from this list. |

## Generations and earning windows

- Generation 1 starts the chain. Each later generation numbers one higher and links the previous one's hash.
- A new generation must move forward: its from epoch must be later than the previous generation's, and its subnet and hotkey must match the chain.
- Generation 1 takes its subnet (chain, genesis hash and netuid) from the authenticated operator. Later generations keep it.
- Without epoch flags, generation 1 covers epochs 0 through 65535. A later generation starts at the operator's current epoch plus 1, or just after the previous generation's start if that is later.
- A delegation starts at the operator's current epoch plus 2, or just after the previous delegation's start if that is later.
- Every window ends 65,535 epochs after its start, with or without the epoch flags.

## Safety

- **The signer only signs two things**: a hotkey wallet consent or a hotkey delegation. Anything else fails with `refusing to sign a message that is neither a hotkey wallet consent nor a hotkey delegation`.
- **A hostile operator cannot trick it.** A delegation challenge is signed only after the protocol decoder accepts it and it names exactly the requested hotkey, consent head, epochs and network.
- **Seed files are read, never created.** They must be private files you own. They are never logged. A missing or unsafe seed file is an error that names its path.
- **Linux and macOS only.** Seed custody does not support Windows. There a seed load fails with `private seed file custody is unsupported on this platform`. `status` without `--hotkey_seed_file` reads no seed and works anywhere.

For the wider Subnet 25 picture see [Bittensor Operations](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Operations). For plain meanings of the words, see the [Glossary](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Glossary).
