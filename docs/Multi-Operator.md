# Multi-Operator Providers

> [!TIP]
> **Most operators can stop here.** Only one operator is listed today (`bringyour.com`), so you do not need multi-operator mode. Also note that signing in with a hotkey (`--auto-register --hotkey_seed_file`) creates a **new** URnetwork account owned by the hotkey at each operator. Your website account and its wallet consent do not cover it. Use [hotkey delegation](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Hotkey-Delegation) or sign a network consent for that account. See the [Quick Start](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Quick-Start).


Run one provider for each operator in the published operator list, from a single supervisor. [Bittensor operations](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Operations) covers the wider Subnet 25 surface, and [Bittensor hotkey delegation](/full-bars/urnetwork-3.23-fix/wiki/Bittensor-Hotkey-Delegation) covers paying one coldkey across the operators.

## What it is

The published operator list says where each URNetwork operator serves its API and Connect endpoints. `provider provide --all-operators` fetches the list and runs this same binary as one child provider per listed operator, each with its own home directory and network token. `provider operators` shows the list and the credential state of each operator.

```text
provider operators [--operators-url=<url>] [-v...]
provider provide --all-operators [--operators-url=<url>] [--operators-refresh=<duration>]
    [--auto-register --hotkey_seed_file=<file> | --hotkey_seed_file=<file>]
    [--port=<port>] [--max-memory=<mem>] [--allow-client-registration]
    [-v...]
```

## `provider operators`

The command prints the list's source and digest first: `source: fetched <url>` or `source: cached <path>, fetched <time>`, then `digest: sha256:<hex>`.

Each listed operator follows as a row with its domain, its credential state (`jwt present` or `awaiting auth`), and its API and Connect URLs. When any operator awaits auth, the command prints where its network token goes: `~/.urnetwork/operators/<domain>/.urnetwork/jwt`.

`--operators-url` points the command at a different list; without it the built-in default list is used.

## `provider provide --all-operators`

Each listed operator gets one child provider, started from this binary with the operator's API and Connect URLs from the list. The operator directory is the child's home, and the child's provider state lives under `.urnetwork` inside it; any inherited `HOME` (and the Windows equivalents) is dropped and replaced.

The network token the supervisor watches is `operators/<domain>/.urnetwork/jwt`. An operator without that token is reported as `awaiting auth`, gets no child, and is rechecked every 5 minutes, so it starts as soon as its token is written.

`--operators-refresh` sets how often the list is refetched: a Go duration of at least 1m, default 1h. `--api_url` is refused with `--all-operators`, because every operator's URLs come from the list.

## The trust model of the list

The list is fetched from `--operators-url`: https, or http only for a literal loopback host. Omitted, the built-in default list is used.

The last good copy is kept as `operators.yml` in the provider state directory, so the commands work while the list host is unreachable; `provider operators` falls back to it when a fetch fails, and a running supervisor keeps the current list.

The list is size-bounded: 1 to 64 KiB, 1 to 256 operators, one strict YAML document with the `urnetwork-operators-v1` schema. Its digest is printed so two boxes can tell which list they follow.

Every operator's own API URL comes from the list. The list only says where each operator serves its endpoints; it grants no weight, payout or registration authority.

## In Docker

The container entrypoint maps environment variables onto the flags above, so no custom command line is needed. With none of them set, the container runs the same `provide` as before.

| Environment variable | Effect |
|---|---|
| `ALL_OPERATORS=1` | Adds `--all-operators` and skips the single `USER_AUTH`/`PASSWORD` login. |
| `AUTO_REGISTER=1` | Adds `--auto-register`; needs `HOTKEY_SEED_FILE`. |
| `HOTKEY_SEED_FILE=/path` | Adds `--hotkey_seed_file=/path`. Mount the seed read-only. |
| `ALLOW_CLIENT_REGISTRATION=auto\|Y\|N` | `Y` adds `--allow-client-registration`; `N` does not; `auto` adds it only when `/root/.urnetwork` has neither `.provider.key` nor an `operators/` directory. |
| `MAX_MEMORY=2gib` | Adds `--max-memory=2gib`; the supervisor splits it across the operator children. |
| `PROVIDER_ARGS="--operators-url=... --operators-refresh=30m"` | Appends those arguments to `provide`, split on whitespace. |

```bash
docker run -d \
  --name=urnetwork-multi \
  -e ALL_OPERATORS=1 \
  -e AUTO_REGISTER=1 \
  -e HOTKEY_SEED_FILE=/run/secrets/hotkey.seed \
  -e ALLOW_CLIENT_REGISTRATION=auto \
  -v urnetwork_multi:/root/.urnetwork \
  -v /etc/urnetwork/hotkey.seed:/run/secrets/hotkey.seed:ro \
  ghcr.io/full-bars/urnetwork-3.23-fix:latest
```

`auto` is meant for the first start on a fresh volume. Once `operators/` exists, an operator added later needs `ALLOW_CLIENT_REGISTRATION=Y` to let its child create a new client identity. Leave `AUTH` unset: it selects the single account login, which the operator supervisor does not use. See [Docker Deployment](/full-bars/urnetwork-3.23-fix/wiki/Docker-Deployment#-multi-operator-flags) for the full variable table.

## State layout

Under the provider state directory (`~/.urnetwork`):

| Path | What it is |
|---|---|
| `operators/<domain>/` | The child provider's home directory. |
| `operators/<domain>/.urnetwork/` | The child's provider state: identity key, client tokens, proxy file, and so on. |
| `operators/<domain>/.urnetwork/jwt` | The operator's network token; the supervisor watches this file. |

Back up the `operators/` tree, or at least each `operators/<domain>/.urnetwork` directory: it holds the child's identity and credentials. A delisted operator keeps its state directory. If `~/.urnetwork/proxy` exists, it is copied into each operator's state directory before the child starts.

## Operating notes

Check the list and credentials with `provider operators`; `jwt present` means the child can start.

Awaiting-auth recovery: write the operator's network token at `operators/<domain>/.urnetwork/jwt`. The supervisor rechecks it every 5 minutes and starts the child as soon as the token is there; no restart of the supervisor is needed.

Refresh: the supervisor takes list changes on the next refresh. New operators start, a child whose URLs changed is restarted with the new URLs, and a delisted operator's child is stopped (SIGTERM, then SIGKILL 60s later) while its state directory stays.

A child that exits restarts after a delay of 30 seconds that doubles up to 10 minutes; a child that ran for 10 minutes or more starts the backoff over.

`SIGINT`, `SIGTERM` and `SIGQUIT` are forwarded to every child, and the supervisor returns once all of them have exited.
