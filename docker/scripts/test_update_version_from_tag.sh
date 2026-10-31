#!/bin/sh
# Regression test: the version recorded after an in-place update must come from
# the RELEASE TAG, never from the download URL.
#
# The bug this locks down: func_check_update derived the version to store by
# scraping `.../download/v<tag>/...` out of the asset URL. Any URL of a different
# shape passed through sed unchanged, so version.txt received the whole URL. The
# next check then compared that string against the real tag, decided the box was
# out of date, and re-downloaded the archive forever - silently. The code comment
# above the *compare* records the same failure happening once already.
#
# No network: `curl` is stubbed, so the test exercises the real
# func_check_update end to end against a fixture release.
#
# Run: sh docker/scripts/test_update_version_from_tag.sh
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

FAILS=0
t() { # t <name> <cmd...>
    name="$1"
    shift
    if "$@"; then
        printf 'PASS  %s\n' "$name"
    else
        printf 'FAIL  %s\n' "$name"
        FAILS=$((FAILS + 1))
    fi
}
# assertions are evaluated inside t, never as a bare command (a bare `[` followed
# by `t x true` reports PASS even when the assertion failed)
assert_eq() { [ "$2" = "$3" ]; }

TAG="v3.23.0-fix.99.9"
BARE="3.23.0-fix.99.9"

# --- fixture: a real tarball carrying linux/amd64/provider -------------------
mkdir -p "$WORK/build/linux/amd64"
printf '#!/bin/sh\necho fixture-provider\n' > "$WORK/build/linux/amd64/provider"
chmod +x "$WORK/build/linux/amd64/provider"
tar -czf "$WORK/asset.tar.gz" -C "$WORK/build" linux/amd64/provider
DIGEST=$(sha256sum "$WORK/asset.tar.gz" | cut -d' ' -f1)

# The download URLs deliberately do NOT match /download/v<tag>/ - that is the
# whole point. A version-parser that reads these must fail the test.
FAT_URL="https://downloads.example.test/assets/provider.tar.gz"
ARCH_URL="https://downloads.example.test/assets/provider-linux-amd64.tar.gz"

cat > "$WORK/releases.json" <<EOF
[{"tag_name":"$TAG","prerelease":false,"draft":false,"assets":[
  {"name":"urnetwork-provider-$TAG.tar.gz","browser_download_url":"$FAT_URL","digest":"sha256:$DIGEST"},
  {"name":"urnetwork-provider-$TAG-linux-amd64.tar.gz","browser_download_url":"$ARCH_URL","digest":"sha256:$DIGEST"}
]}]
EOF

# --- harness: source the real script, stub curl ------------------------------
PROVIDER_MODE=nightly
PROVIDER_FLAVOR=nightly
A_SYS_ARCH=amd64
export PROVIDER_MODE PROVIDER_FLAVOR A_SYS_ARCH
. "$HERE/provider_common.sh"

APP="$WORK/app"
HOME_FIX="$WORK/home"
mkdir -p "$APP" "$HOME_FIX"
APP_DIR="$APP"
VERSION_FILE="$APP/version.txt"
HOME="$HOME_FIX"
export HOME

curl() {
    for a in "$@"; do
        case "$a" in
            *releases?*) cat "$WORK/releases.json"; return 0 ;;
        esac
    done
    # download call: find the -o target and copy the fixture tarball there
    prev=""
    for a in "$@"; do
        if [ "$prev" = "-o" ]; then
            cp "$WORK/asset.tar.gz" "$a"
            return 0
        fi
        prev="$a"
    done
    return 0
}

printf '%s\n' "3.23.0-fix.30.0" > "$VERSION_FILE"
func_check_update > "$WORK/run.log" 2>&1

RECORDED=$(cat "$VERSION_FILE" 2>/dev/null)

# 1. the recorded version is the tag, WITHOUT the leading v (the compare
#    normalises the tag with sed 's/^v//', so storing the bare form keeps them
#    in agreement)
t "records the release tag, not the url" assert_eq x "$RECORDED" "$BARE"

# 2. it must not be the url, and must not be empty
t "recorded version is not the download url" assert_eq x "$([ "$RECORDED" = "$ARCH_URL" ] && echo url || echo ok)" "ok"
t "recorded version is not empty" assert_eq x "$([ -n "$RECORDED" ] && echo ok || echo empty)" "ok"

# 3. the update actually happened
t "installed the provider binary" assert_eq x "$([ -x "$APP/urnetwork_amd64_nightly" ] && echo ok || echo missing)" "ok"
t "set the restart marker" assert_eq x "$([ -f "$HOME/.urnetwork/update-pending" ] && echo ok || echo missing)" "ok"

# 4. and a second run now SKIPS rather than re-downloading - the actual user
#    symptom the bug caused
func_check_update > "$WORK/run2.log" 2>&1
t "second run skips: already at latest" assert_eq x "$(grep -c 'Already at latest provider version' "$WORK/run2.log")" "1"

# 5. self-healing: a box whose version.txt already holds a URL (written by the
#    old scraping code) must recover - one more download, then the bare tag,
#    then it skips
rm -f "$HOME/.urnetwork/update-pending" "$APP/urnetwork_amd64_nightly"
printf '%s\n' "$ARCH_URL" > "$VERSION_FILE"
func_check_update > "$WORK/heal.log" 2>&1
t "self-heal: url in version.txt is replaced by the bare tag" assert_eq x "$(cat "$VERSION_FILE")" "$BARE"
t "self-heal: re-downloaded and reinstalled the binary" assert_eq x "$([ -x "$APP/urnetwork_amd64_nightly" ] && echo ok || echo missing)" "ok"
func_check_update > "$WORK/heal2.log" 2>&1
t "self-heal: next run skips: already at latest" assert_eq x "$(grep -c 'Already at latest provider version' "$WORK/heal2.log")" "1"

# 6. a selected release whose tag resolves to nothing must abort BEFORE any
#    destructive step. Runs last, in the same shell as the cases above, so a
#    LATEST_VERSION_CHECK left over from a previous release would be caught:
#    the stale value would be recorded and a binary installed.
printf '#!/bin/sh\necho old-provider\n' > "$APP/urnetwork_amd64_nightly"
chmod +x "$APP/urnetwork_amd64_nightly"
BIN_SUM_BEFORE=$(cksum < "$APP/urnetwork_amd64_nightly")
printf '%s\n' "3.23.0-fix.30.0" > "$VERSION_FILE"
rm -f "$HOME/.urnetwork/update-pending"
# tag "v" survives release selection (assets are named after the tag) and
# normalises to the empty string once the leading v is stripped
sed "s/$TAG/v/g" "$WORK/releases.json" > "$WORK/releases-notag.json"
cp "$WORK/releases.json" "$WORK/releases-good.json"
cp "$WORK/releases-notag.json" "$WORK/releases.json"
func_check_update > "$WORK/notag.log" 2>&1
cp "$WORK/releases-good.json" "$WORK/releases.json"
t "empty tag: update aborts" assert_eq x "$(grep -c 'refusing to install an unversioned provider' "$WORK/notag.log")" "1"
t "empty tag: installed binary bytes untouched" assert_eq x "$(cksum < "$APP/urnetwork_amd64_nightly")" "$BIN_SUM_BEFORE"
t "empty tag: version.txt untouched" assert_eq x "$(cat "$VERSION_FILE")" "3.23.0-fix.30.0"
t "empty tag: no restart marker" assert_eq x "$([ -e "$HOME/.urnetwork/update-pending" ] && echo set || echo none)" "none"

# --- no jq: the release selector needs jq, so the updater must SKIP safely --
# provider_common.sh parses the tag and selects the asset through jq. Without it
# there is no safe way to know what to install, so the contract is "skip and
# change nothing" - never "install something half-understood". Force the case by
# hiding jq behind a PATH of symlinks to every other tool.
NOJQ="$WORK/nojq-bin"
mkdir -p "$NOJQ"
# a minimal allowlist, NOT a copy of every tool on PATH: linking thousands of
# binaries is slow and can exhaust limits mid-loop, which fails the test for a
# reason that has nothing to do with the code under test
for _t in sh sed grep tar cp mv cksum mktemp date ln cat awk tr head tail sort id \
          dirname basename chmod mkdir rm touch sleep sha256sum find printf env; do
    _p=$(command -v "$_t" 2>/dev/null) || continue
    ln -sf "$_p" "$NOJQ/$_t" 2>/dev/null
done
if PATH="$NOJQ" command -v jq >/dev/null 2>&1; then
    printf 'SKIP  no-jq case (jq could not be hidden)\n'
else
    PATH_SAVE=$PATH
    PATH="$NOJQ"
    func_check_update > "$WORK/nojq.log" 2>&1
    PATH=$PATH_SAVE
    t "no jq: update skips instead of installing blind" assert_eq x "$(grep -c 'Update skipped' "$WORK/nojq.log")" "1"
    t "no jq: installed binary untouched" assert_eq x "$(cksum < "$APP/urnetwork_amd64_nightly")" "$BIN_SUM_BEFORE"
    t "no jq: version.txt untouched" assert_eq x "$(cat "$VERSION_FILE")" "3.23.0-fix.30.0"
fi

if [ "$FAILS" -ne 0 ]; then
    printf '\n%d assertion(s) failed\n' "$FAILS"
    printf '\n--- run.log ---\n'; cat "$WORK/run.log"
    printf '\n--- notag.log ---\n'; cat "$WORK/notag.log" 2>/dev/null
    exit 1
fi
printf '\nall assertions passed\n'
