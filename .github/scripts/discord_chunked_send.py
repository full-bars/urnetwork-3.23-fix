#!/usr/bin/env python3
"""Send a Discord embed, splitting the description into multiple messages if
it exceeds Discord's 4096-char embed limit.

Reads a full webhook payload JSON ({"embeds": [...]}) from the file path given
as argv[1], chunks each embed's description at newline boundaries (max 4000
chars per chunk), and POSTs each chunk as a separate embed via the webhook URL
in the DISCORD_WEBHOOK env var. All chunks share the same title/color/footer so
they read as one alert.

Exits non-zero if any POST fails, and logs the HTTP status of every POST so
failures are visible in CI logs instead of silently dropping the alert.
"""
import json
import os
import sys
import urllib.error
import urllib.request

MAX_CHUNK = 4000  # under Discord's 4096 limit; leaves room for other fields


def chunk_description(desc: str) -> list[str]:
    chunks = []
    while len(desc) > MAX_CHUNK:
        cut = desc.rfind("\n", 0, MAX_CHUNK)
        if cut == -1 or cut < MAX_CHUNK // 2:
            cut = MAX_CHUNK
        chunks.append(desc[:cut])
        desc = desc[cut:].lstrip("\n")
    if desc:
        chunks.append(desc)
    return chunks


def main() -> int:
    if len(sys.argv) < 2:
        print("[DISCORD] error: expected payload JSON file path as argv[1]", file=sys.stderr)
        return 1
    webhook = os.environ.get("DISCORD_WEBHOOK", "")
    if not webhook:
        print("[DISCORD] error: DISCORD_WEBHOOK not set", file=sys.stderr)
        return 1

    with open(sys.argv[1], "r", encoding="utf-8") as f:
        payload = json.load(f)

    embeds = payload.get("embeds", [])
    if not embeds:
        print("[DISCORD] error: payload has no embeds", file=sys.stderr)
        return 1

    # Split every embed's description; an embed that fits stays a single chunk.
    chunked_embeds: list[dict] = []
    for embed in embeds:
        desc = embed.get("description", "")
        chunks = chunk_description(desc)
        for i, chunk in enumerate(chunks, start=1):
            e = dict(embed)
            e["description"] = chunk
            if len(chunks) > 1:
                # Label follow-ups so it's clear the alert continues
                footer = e.get("footer", {})
                if isinstance(footer, dict):
                    footer = dict(footer)
                    footer["text"] = f"{footer.get('text', '')} • {i}/{len(chunks)}".strip(" •")
                    e["footer"] = footer
            chunked_embeds.append(e)

    ok = True
    total = len(chunked_embeds)
    for i, e in enumerate(chunked_embeds, start=1):
        body = json.dumps({"embeds": [e]}).encode("utf-8")
        req = urllib.request.Request(
            webhook,
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                print(f"[DISCORD] POST part {i}/{total}: HTTP {resp.status}")
        except urllib.error.HTTPError as err:
            detail = err.read(200).decode("utf-8", "replace")
            print(f"[DISCORD] ERROR part {i}/{total}: HTTP {err.code} {detail}", file=sys.stderr)
            ok = False
        except Exception as err:  # noqa: BLE001 - report and continue
            print(f"[DISCORD] ERROR part {i}/{total}: {err}", file=sys.stderr)
            ok = False

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
