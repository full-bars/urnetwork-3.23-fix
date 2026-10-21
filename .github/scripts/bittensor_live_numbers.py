#!/usr/bin/env python3
"""Refresh the SN25 live-numbers block in the Bittensor head-tier page.

The block between the two HTML comment markers in docs/Bittensor-Head-Tier.md is
generated from live chain state. Nothing else in the page is touched.

Sources:
  - Substrate RPC on https://lite.chain.opentensor.ai for the netuid 25
    registration economics. The storage keys below are twox128(pallet) ++
    twox128(item) ++ blake2_128_concat(SCALE(netuid)) for netuid 25, which is a
    constant; they are precomputed so this script needs no hashing libraries.
  - The EVM JSON-RPC on the same host (chain 964) for the coordinator, the
    commitment oracle and gas.
  - A spot TAO price, CoinGecko first and Binance as a fallback.

Usage:
  python3 .github/scripts/bittensor_live_numbers.py --write   # rewrite the page
  python3 .github/scripts/bittensor_live_numbers.py --check   # exit 1 if stale
  python3 .github/scripts/bittensor_live_numbers.py --print   # print the block

The date line only moves when a value moves, so an unchanged week is not a
commit.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

RPC = "https://lite.chain.opentensor.ai"
CHAIN_ID = 964
NETUID = 25
SUBNET_UIDS = 256
COORDINATOR = "0xC18925925E2B7bb9059b7d696b8c92762AE86406"
ORACLE = "0x56Ddfb8f3E267E98EfDa690110645f31365BCF03"
BLOCK_SECONDS = 12

# Measured on 2026-10-21 with `forge test --match-test
# test_manyClientsCanBindOneFleetAndBecomeActiveAtBoundary --gas-report` against
# evm/test/ReleaseBindingPolicy.t.sol (avg of 3 calls). Re-measure when the
# coordinator changes.
BIND_GAS = 245_301

PAGE = Path("docs/Bittensor-Head-Tier.md")
BEGIN = "<!-- BEGIN SN25 LIVE NUMBERS (generated) -->"
END = "<!-- END SN25 LIVE NUMBERS (generated) -->"

# twox128("SubtensorModule") ++ twox128(item) ++ SCALE(netuid 25, u16 LE).
# The netuid maps use the Identity hasher, so the key carries the raw two bytes
# with no hash prefix. These are constants for netuid 25 and never change.
STORAGE_KEYS = {
    "burn": "0x658faa385070e074c85bf6b568cf055501be1755d08418802946bca51b6863251900",
    "min_burn": "0x658faa385070e074c85bf6b568cf0555b5ebf5d60c129a116973e38cd593e6e61900",
    "max_burn": "0x658faa385070e074c85bf6b568cf0555113e5f8ae432b71a8c1ac60fd9d6ff2e1900",
    "uids": "0x658faa385070e074c85bf6b568cf0555a1048e9d244171852dfe8db314dc68ca1900",
    "immunity": "0x658faa385070e074c85bf6b568cf0555b6522cfe03433e9e101a258ee2f580ab1900",
    "tempo": "0x658faa385070e074c85bf6b568cf05557641384bb339f3758acddfd7053d33171900",
    "registration_allowed": "0x658faa385070e074c85bf6b568cf0555d5fe74da02c7b4bbb340fb368eee3e771900",
}


def rpc(method: str, params: list) -> Any:
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    last = ""
    for attempt in range(4):
        request = urllib.request.Request(
            RPC, data=body, headers={"Content-Type": "application/json", "User-Agent": "urnetwork-docs"}
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read())
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as err:
            last = f"{method}: {err}"
            time.sleep(2 * (attempt + 1))
            continue
        if "error" in payload:
            last = f"{method}: {json.dumps(payload['error'])[:300]}"
            time.sleep(2 * (attempt + 1))
            continue
        return payload["result"]
    raise SystemExit(f"rpc failed after retries ({last})")


def storage(name: str) -> bytes:
    key = STORAGE_KEYS[name]
    value = rpc("state_getStorage", [key])
    if not value or value == "0x":
        raise SystemExit(f"storage {name} is empty")
    return bytes.fromhex(value[2:])


def storage_optional(name: str) -> bytes | None:
    key = STORAGE_KEYS[name]
    value = rpc("state_getStorage", [key])
    if not value or value == "0x":
        return None
    return bytes.fromhex(value[2:])


def u64(key: str) -> int:
    return int.from_bytes(storage(key), "little")


def eth_call(data: str, to: str = COORDINATOR) -> str:
    return rpc("eth_call", [{"to": to, "data": data}, "latest"])


def tao(rao: float) -> str:
    text = f"{rao / 1e9:.6f}".rstrip("0").rstrip(".")
    return text or "0"


def usd(value: float) -> str:
    return f"{value:,.2f}"


def tao_price() -> float:
    endpoints = [
        (
            "https://api.coingecko.com/api/v3/simple/price?ids=bittensor&vs_currencies=usd",
            lambda payload: payload["bittensor"]["usd"],
        ),
        (
            "https://api.binance.com/api/v3/ticker/price?symbol=TAOUSDT",
            lambda payload: payload["price"],
        ),
    ]
    for url, extract in endpoints:
        try:
            request = urllib.request.Request(url, headers={"User-Agent": "urnetwork-docs"})
            with urllib.request.urlopen(request, timeout=20) as response:
                price = float(extract(json.loads(response.read())))
        except Exception:
            continue
        if price > 0:
            return price
    raise SystemExit("could not read a TAO price")


def read_state() -> dict:
    burn_rao = u64("burn")
    min_burn_rao = u64("min_burn")
    max_burn_raw = storage_optional("max_burn")
    max_burn_rao = int.from_bytes(max_burn_raw, "little") if max_burn_raw else None
    uids = int.from_bytes(storage("uids"), "little")
    immunity = u64("immunity")
    tempo = u64("tempo")
    registration_allowed = storage("registration_allowed")[0] == 1

    chain_id = int(rpc("eth_chainId", []), 16)
    if chain_id != CHAIN_ID:
        raise SystemExit(f"unexpected EVM chain id {chain_id}, expected {CHAIN_ID}")
    gas_price_wei = int(rpc("eth_gasPrice", []), 16)
    oracle_balance_wei = int(rpc("eth_getBalance", [ORACLE, "latest"]), 16)
    oracle = "0x" + eth_call("0x06d902e9")[-40:]
    paused = int(eth_call("0x5c975abb"), 16) == 1

    if not 0 < burn_rao:
        raise SystemExit(f"implausible burn {burn_rao} rao")
    if max_burn_rao is not None and burn_rao > max_burn_rao:
        raise SystemExit(f"burn {burn_rao} rao is above the ceiling {max_burn_rao}")
    if not 0 < uids <= 4096:
        raise SystemExit(f"implausible seat count {uids}")
    if immunity <= 0 or tempo <= 0:
        raise SystemExit("implausible immunity or tempo")

    return {
        "burn_rao": burn_rao,
        "min_burn_rao": min_burn_rao,
        "max_burn_rao": max_burn_rao,
        "uids": uids,
        "immunity": immunity,
        "tempo": tempo,
        "registration_allowed": registration_allowed,
        "gas_price_wei": gas_price_wei,
        "oracle_balance_wei": oracle_balance_wei,
        "oracle_is_active": oracle.lower() == ORACLE.lower(),
        "paused": paused,
        "tao_usd": tao_price(),
    }


def render_body(state: dict) -> str:
    price = state["tao_usd"]
    per_bind_tao = BIND_GAS * state["gas_price_wei"] / 1e18
    seat_usd = state["burn_rao"] / 1e9 * price
    bind_usd = per_bind_tao * price
    immunity_days = state["immunity"] * BLOCK_SECONDS / 86400
    tempo_minutes = state["tempo"] * BLOCK_SECONDS / 60

    rows = [
        ("Fleet seat registration burn", f"{tao(state['burn_rao'])} TAO (~${usd(seat_usd)})"),
        ("Burn floor", f"{tao(state['min_burn_rao'])} TAO"),
        ("Seats in use", f"{state['uids']} of {SUBNET_UIDS}"),
        ("Registration open", "yes" if state["registration_allowed"] else "no"),
        ("Immunity after registering", f"{state['immunity']:,} blocks (~{immunity_days:.1f} days)"),
        ("Tempo", f"{state['tempo']:,} blocks (~{tempo_minutes:.0f} minutes)"),
        ("Gas price, chain 964", f"{state['gas_price_wei'] / 1e9:.1f} gwei"),
        ("One node binding", f"{BIND_GAS:,} gas, about {per_bind_tao:.6f} TAO (~${usd(bind_usd)})"),
        ("Commitment oracle", f"active, balance {state['oracle_balance_wei'] / 1e18:.4f} TAO" if state["oracle_is_active"] else "NOT the coordinator's active oracle"),
        ("Coordinator", "paused" if state["paused"] else "not paused"),
    ]

    lines = [
        "| | Value |",
        "|---|---|",
    ]
    lines += [f"| {name} | {value} |" for name, value in rows]
    return "\n".join(lines)


def render_block(body: str, changed_on: str) -> str:
    return "\n".join([
        BEGIN,
        f"_Live values · last changed {changed_on} UTC · checked hourly_",
        "",
        body,
        END,
    ])


def existing_block(text: str) -> str | None:
    start = text.find(BEGIN)
    end = text.find(END)
    if start < 0 or end < start:
        return None
    return text[start:end + len(END)]


def normalized(block: str) -> str:
    """Ignore the date so an unchanged hour is not a change, but a changed
    header or body still is."""
    return re.sub(r"\d{4}-\d{2}-\d{2}(?: \d{2}:\d{2})?", "<date>", block).strip()


def main() -> int:
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true")
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--print", dest="print_block", action="store_true")
    args = parser.parse_args()

    state = read_state()
    body = render_body(state)
    today = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M")

    if args.print_block:
        print(render_block(body, today))
        return 0

    if not PAGE.exists():
        raise SystemExit(f"{PAGE} is missing")
    text = PAGE.read_text()
    current = existing_block(text)
    if current is None:
        raise SystemExit(f"{PAGE} has no generated block markers")

    unchanged = normalized(current) == normalized(render_block(body, "<date>"))
    if args.check:
        if unchanged:
            print("live numbers are current")
            return 0
        print("live numbers are stale; run with --write", file=sys.stderr)
        return 1

    if unchanged:
        print("unchanged")
        return 0

    PAGE.write_text(text.replace(current, render_block(body, today)))
    print("updated")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
