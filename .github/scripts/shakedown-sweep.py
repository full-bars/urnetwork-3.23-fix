#!/usr/bin/env python3
"""Shakedown droplet sweeper — out-of-band orphan guarantee.

The shakedown workflow deletes its own droplet on every normal path, but a
runner eviction, job-timeout SIGKILL, or DO API failure can orphan a droplet
with no one to delete it. DigitalOcean has no built-in auto-expire.

This script is the backstop. It lists all droplets tagged shakedown-ci and
destroys any older than the cutoff, then reaps stale shakedown-ci SSH keys.

Usage: shakedown-sweep.py [--max-age-hours N]
Env:  DIGITAL_OCEAN_TOKEN (the DO API token)
"""

import datetime
import json
import os
import sys
import urllib.error
import urllib.request

API = "https://api.digitalocean.com/v2"
DEFAULT_MAX_AGE_HOURS = 3.0


def req(path, method="GET", auth=""):
    r = urllib.request.Request(API + path, method=method, headers={"Authorization": auth})
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            body = resp.read()
            return resp.status, json.loads(body) if body else {}
    except urllib.error.HTTPError as e:
        return e.code, {}
    except Exception as e:
        return getattr(e, "code", 0), {}


def main():
    token = os.environ.get("DIGITAL_OCEAN_TOKEN", "")
    if not token:
        print("::error::DIGITAL_OCEAN_TOKEN not set — cannot sweep")
        return 1
    auth = "Bearer " + token

    max_hours = DEFAULT_MAX_AGE_HOURS
    for arg in sys.argv[1:]:
        if arg.startswith("--max-age-hours="):
            max_hours = float(arg.split("=", 1)[1])
    cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=max_hours)

    # --- Sweep droplets tagged shakedown-ci ---
    page = 1
    found = 0
    destroyed = 0
    list_failed = False
    while True:
        code, data = req("/droplets?tag_name=shakedown-ci&per_page=200&page={}".format(page), auth=auth)
        if code != 200:
            print("::error::list droplets failed HTTP {}".format(code))
            list_failed = True
            break
        droplets = data.get("droplets", [])
        if not droplets:
            break
        for d in droplets:
            found += 1
            created = datetime.datetime.fromisoformat(d["created_at"].replace("Z", "+00:00"))
            if created < cutoff:
                did = d["id"]
                code, _ = req("/droplets/{}".format(did), method="DELETE", auth=auth)
                print("Sweep: destroy {} ({}, created {}) HTTP {}".format(did, d["name"], d["created_at"], code))
                if code in (200, 204):
                    destroyed += 1
            else:
                print("Sweep: keep {} ({}) — age within {}h".format(d["id"], d["name"], max_hours))
        page += 1
        if page > 50:  # 10k droplets max, safety
            break
    print("Sweep summary: found={} destroyed={}".format(found, destroyed))
    # A failed list means the sweep could not verify — fail LOUD so the
    # scheduled job shows red instead of silently-green with orphans at risk
    # (DS R3 S4).
    if list_failed:
        return 1

    # --- Reap stale shakedown-ci SSH keys (paginated) ---
    # DO SSH key objects expose id/fingerprint/public_key/name but NOT
    # created_at, and droplets do NOT carry their ssh_keys back in GET
    # responses (Fable5 SF-5 — the fingerprint-matching approach was dead
    # code). The key NAME embeds the creation epoch (shakedown-ci-<epoch>,
    # set at create in shakedown.yml), so parse it and reap only keys older
    # than the cutoff. This protects in-progress runs.
    key_page = 1
    while True:
        code, data = req("/account/keys?per_page=200&page={}".format(key_page), auth=auth)
        if code != 200:
            # Fail loud, same as the droplet list: stale keys accumulating under
            # a green scheduled job is a silent drift (CodeRabbit).
            print("::error::list ssh keys failed HTTP {}".format(code))
            return 1
        keys = data.get("ssh_keys", [])
        if not keys:
            break
        for k in keys:
            name = k.get("name", "")
            if not name.startswith("shakedown-ci-"):
                continue
            epoch = name[len("shakedown-ci-"):]
            try:
                created = datetime.datetime.fromtimestamp(int(epoch), tz=datetime.timezone.utc)
            except (ValueError, OverflowError):
                # Unparseable epoch: cannot prove it is old. Keep it (safer
                # than deleting a key we cannot age).
                print("Sweep: keep key {} ({}) — unparseable epoch".format(k["id"], name))
                continue
            if created < cutoff:
                code, _ = req("/account/keys/{}".format(k["id"]), method="DELETE", auth=auth)
                print("Sweep: remove stale key {} ({}, created {}) HTTP {}".format(k["id"], name, created.isoformat(), code))
            else:
                print("Sweep: keep key {} ({}) — age within {}h".format(k["id"], name, max_hours))
        key_page += 1
        if key_page > 50:
            break
    print("Sweep: key reap done")
    return 0


if __name__ == "__main__":
    sys.exit(main())
