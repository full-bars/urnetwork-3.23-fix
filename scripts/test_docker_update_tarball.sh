#!/bin/bash
# Tests for the docker/scripts/urnet-tools.sh "update" operation's tarball
# handling.
#
# Regression coverage for the update tarball handling. It replaced the fixed,
# predictable /tmp/urnetwork-update.tar.gz path with a random temp DIR created
# via `mktemp -d /tmp/urnetwork-update-XXXXXX` (the XXXXXX suffix is required:
# busybox mktemp rejects templates that end in anything else, e.g. a .tar.gz
# suffix). The tarball lives inside that dir as update.tar.gz, and every
# cleanup path removes the dir with rm -rf.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ORIG_SCRIPT="$REPO_ROOT/docker/scripts/urnet-tools.sh"

TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

FAILS=0

# --- Assertion helpers (mirrors conventions used elsewhere in scripts/) ---

assert_eq() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected: '$expected'"
        echo "     Actual:   '$actual'"
        FAILS=$((FAILS + 1))
    fi
}

assert_exit_code() {
    local expected="$1" actual="$2" msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  ✅ PASS: $msg (exit=$actual)"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Expected exit: $expected, actual: $actual"
        FAILS=$((FAILS + 1))
    fi
}


assert_file_absent() {
    local file="$1" msg="$2"
    if [ ! -e "$file" ]; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Path '$file' should not exist but does."
        FAILS=$((FAILS + 1))
    fi
}

assert_matches() {
    local value="$1" pattern="$2" msg="$3"
    if printf '%s' "$value" | grep -Eq "$pattern"; then
        echo "  ✅ PASS: $msg"
    else
        echo "  ❌ FAIL: $msg"
        echo "     Value '$value' did not match pattern '$pattern'"
        FAILS=$((FAILS + 1))
    fi
}

# --- Test fixture setup ---
#
# The real script hardcodes provider_bin="/app/urnetwork_${arch}_stable",
# which requires write access to /app. We don't have (and shouldn't need)
# root, so we test against a copy of the script with that one prefix
# rewritten to a writable temp dir. The tarball/mktemp/cleanup logic under
# test is untouched by this substitution.
APP_DIR="$TEMP_DIR/app"
mkdir -p "$APP_DIR"
SCRIPT_COPY="$TEMP_DIR/urnet-tools.sh"
sed "s#/app/urnetwork_#${APP_DIR}/urnetwork_#" "$ORIG_SCRIPT" > "$SCRIPT_COPY"
chmod +x "$SCRIPT_COPY"

# A deliberately-regressed sibling copy: the staged_provider mktemp template
# gets a ".new" suffix appended after XXXXXX, mirroring the exact class of
# bug this PR fixed for the tarball mktemp call, but on the OTHER mktemp
# call in do_update. Used to prove the MOCKBIN/mktemp busybox stub actually
# enforces the "XXXXXX must be last" rule everywhere it's invoked, not just
# for the tarball path.
REGRESSED_STAGED_SCRIPT="$TEMP_DIR/urnet-tools-regressed-staged.sh"
sed 's#mktemp "${provider_bin}.XXXXXX"#mktemp "${provider_bin}.XXXXXX.new"#' "$SCRIPT_COPY" > "$REGRESSED_STAGED_SCRIPT"
chmod +x "$REGRESSED_STAGED_SCRIPT"
if ! grep -q 'mktemp "${provider_bin}.XXXXXX.new"' "$REGRESSED_STAGED_SCRIPT"; then
    echo "❌ FATAL: failed to construct regressed staged_provider script copy (script may have changed)"
    exit 1
fi

# busybox mktemp requires XXXXXX to be the LAST characters of the template; a
# suffix (like the old ".tar.gz") makes it fail with "Invalid argument". The
# script must create ONE temp DIR (mktemp -d, XXXXXX at the end) and place the
# tarball inside it. Assert the broken .tar.gz-suffixed template is GONE and
# the busybox-safe mktemp -d form is present.
if grep -q 'mktemp /tmp/urnetwork-update-XXXXXX.tar.gz' "$SCRIPT_COPY"; then
    echo "❌ FATAL: busybox-incompatible .tar.gz-suffixed mktemp template still present in script copy"
    exit 1
fi
# Anchor the positive check to the ACTUAL executable assignment, not a bare
# string match that a comment or doc line could also satisfy (coderabbit).
if ! grep -q 'tmpdir="$(mktemp -d /tmp/urnetwork-update-XXXXXX)"' "$SCRIPT_COPY"; then
    echo "❌ FATAL: expected busybox-safe tmpdir mktemp -d assignment not found in script copy (script may have changed)"
    exit 1
fi

MOCKBIN="$TEMP_DIR/mockbin"
mkdir -p "$MOCKBIN"

# Opus hardening: a busybox-enforcing mktemp stub. busybox mktemp requires the
# template to END in XXXXXX; any suffix (e.g. ".tar.gz" or ".new") fails with
# "Invalid argument". GNU coreutils (the host) accepts both, so without this
# stub a regressed template in a non-tarball mktemp call (e.g. staged_provider)
# would silently pass the harness. Delegating to the real mktemp still creates
# the temp file/dir, so all functional assertions stay meaningful.
cat > "$MOCKBIN/mktemp" <<'EOF'
#!/bin/bash
template="${!#}"
case "$template" in
    *XXXXXX) ;;
    *) echo "mktemp: : Invalid argument" >&2; exit 1 ;;
esac
exec "/usr/sbin/mktemp" "$@"
EOF
chmod +x "$MOCKBIN/mktemp"

cat > "$MOCKBIN/curl" <<'EOF'
#!/bin/bash
# Mock curl. Logs every invocation to $CURL_LOG, then emulates the three
# distinct calls the update flow makes:
#   1) release info fetch (no -o flag)              -> prints JSON
#   2/3) tarball download to a file (-o <file> <url>) -> writes dummy bytes,
#        success/failure controlled by MOCK_PRIMARY_FAIL / MOCK_MIRROR_FAIL
#        depending on which host the URL targets.
if [ -n "${CURL_LOG:-}" ]; then
    printf '%s\n' "$*" >> "$CURL_LOG"
fi

outfile=""
prev=""
for arg in "$@"; do
    if [ "$prev" = "-o" ]; then
        outfile="$arg"
    fi
    prev="$arg"
done
url="${!#}"

if [ -z "$outfile" ]; then
    printf '{"tag_name":"%s","assets":[{"name":"urnetwork-linux-amd64.tar.gz","browser_download_url":"%s"}]}' \
        "${MOCK_VERSION:-}" "${MOCK_DOWNLOAD_URL:-}"
    exit 0
fi

case "$url" in
    *dl.fullbars.xyz*)
        if [ "${MOCK_PRIMARY_FAIL:-0}" = "1" ]; then
            # Simulate a partial/truncated download: real curl can write
            # bytes to -o before the transfer dies and it exits non-zero.
            # This lets tests assert the mirror retry overwrites (not
            # appends to) whatever primary already wrote to the same path.
            if [ -n "${MOCK_PRIMARY_PARTIAL_CONTENT:-}" ]; then
                printf '%s' "$MOCK_PRIMARY_PARTIAL_CONTENT" > "$outfile"
            fi
            exit 22
        fi
        printf '%s' "${MOCK_TARBALL_CONTENT:-dummy-tarball-bytes}" > "$outfile"
        exit 0
        ;;
    *github.com*)
        [ "${MOCK_MIRROR_FAIL:-0}" = "1" ] && exit 22
        printf '%s' "${MOCK_TARBALL_CONTENT:-dummy-tarball-bytes}" > "$outfile"
        exit 0
        ;;
    *)
        exit 22
        ;;
esac
EOF

cat > "$MOCKBIN/jq" <<'EOF'
#!/bin/bash
# Mock jq. Ignores stdin content and returns canned values keyed off the
# requested filter, driven by MOCK_VERSION / MOCK_DOWNLOAD_URL.
filter="$2"
cat >/dev/null
case "$filter" in
    *tag_name*)
        printf '%s\n' "${MOCK_VERSION:-}"
        ;;
    *browser_download_url*)
        printf '%s\n' "${MOCK_DOWNLOAD_URL:-}"
        ;;
    *)
        printf '\n'
        ;;
esac
EOF

cat > "$MOCKBIN/tar" <<'EOF'
#!/bin/bash
# Mock tar for `tar -xzf <tarball> -C <tmpdir>`.
tmpdir=""
prev=""
for arg in "$@"; do
    if [ "$prev" = "-C" ]; then
        tmpdir="$arg"
    fi
    prev="$arg"
done

[ "${MOCK_TAR_FAIL:-0}" = "1" ] && exit 1

if [ -n "$tmpdir" ] && [ "${MOCK_TAR_NO_PROVIDER:-0}" != "1" ]; then
    printf '#!/bin/sh\necho mock-provider\n' > "$tmpdir/provider"
    chmod +x "$tmpdir/provider"
fi
exit 0
EOF

cat > "$MOCKBIN/uname" <<'EOF'
#!/bin/bash
if [ "$1" = "-m" ]; then
    printf '%s\n' "${MOCK_ARCH:-x86_64}"
    exit 0
fi
exec /usr/bin/uname "$@"
EOF

cat > "$MOCKBIN/pkill" <<'EOF'
#!/bin/bash
# Real pkill exits non-zero when no process matches; mimic that harmlessly.
exit 1
EOF

cat > "$MOCKBIN/pgrep" <<'EOF'
#!/bin/bash
# Real pgrep exits non-zero when no process matches; harmless for our mocks.
exit 1
EOF

chmod +x "$MOCKBIN"/curl "$MOCKBIN"/jq "$MOCKBIN"/tar "$MOCKBIN"/uname "$MOCKBIN"/pkill "$MOCKBIN"/pgrep

CURL_LOG="$TEMP_DIR/curl.log"

# Snapshot of leftover urnetwork-update-* artifacts directly under /tmp,
# used to assert that every code path cleans up fully regardless of the
# randomized name mktemp assigns.
snapshot_tmp_artifacts() {
    find /tmp -maxdepth 1 -name 'urnetwork-update-*' 2>/dev/null | sort
}

extract_tarball_paths() {
    # Pulls the argument following "-o" out of each logged curl invocation
    # that performed a file download (as opposed to the plain JSON fetch).
    grep -- ' -o ' "$1" 2>/dev/null | sed -E 's/.*-o ([^ ]+).*/\1/'
}

reset_fixture() {
    rm -rf "$APP_DIR"
    mkdir -p "$APP_DIR"
    : > "$CURL_LOG"
    # Reset every mock control flag so state never leaks between tests;
    # each test only needs to set the flags relevant to its scenario.
    MOCK_VERSION=""
    MOCK_DOWNLOAD_URL=""
    MOCK_PRIMARY_FAIL=0
    MOCK_MIRROR_FAIL=0
    MOCK_TAR_FAIL=0
    MOCK_TAR_NO_PROVIDER=0
    MOCK_ARCH=x86_64
    MOCK_TARBALL_CONTENT=""
    MOCK_PRIMARY_PARTIAL_CONTENT=""
    # do_update writes its update-pending marker under "$HOME/.urnetwork".
    # Point HOME at a disposable per-fixture dir so tests never touch the
    # real invoking user's home directory, and so each test starts with a
    # clean marker_dir.
    MOCK_HOME="$TEMP_DIR/home"
    rm -rf "$MOCK_HOME"
    mkdir -p "$MOCK_HOME"
}

run_update() {
    # Runs `urnet-tools update` against the patched script copy with the
    # mock toolchain in front of PATH. Sets $out and $ec as side effects.
    # HOME is pinned to MOCK_HOME (see reset_fixture) so the update-pending
    # marker never lands in the real invoking user's home directory, and an
    # optional SCRIPT override lets tests run a deliberately-mutated copy
    # (e.g. the staged_provider mktemp regression check).
    out="$(
        MOCK_VERSION="${MOCK_VERSION:-}" \
        MOCK_DOWNLOAD_URL="${MOCK_DOWNLOAD_URL:-}" \
        MOCK_PRIMARY_FAIL="${MOCK_PRIMARY_FAIL:-0}" \
        MOCK_MIRROR_FAIL="${MOCK_MIRROR_FAIL:-0}" \
        MOCK_TAR_FAIL="${MOCK_TAR_FAIL:-0}" \
        MOCK_TAR_NO_PROVIDER="${MOCK_TAR_NO_PROVIDER:-0}" \
        MOCK_ARCH="${MOCK_ARCH:-x86_64}" \
        MOCK_PRIMARY_PARTIAL_CONTENT="${MOCK_PRIMARY_PARTIAL_CONTENT:-}" \
        CURL_LOG="$CURL_LOG" \
        HOME="${MOCK_HOME:-$TEMP_DIR/home}" \
        PATH="$MOCKBIN:$PATH" \
        bash "${RUN_SCRIPT:-$SCRIPT_COPY}" update 2>&1
    )" || ec=$?
    ec="${ec:-0}"
}

TARBALL_PATTERN='^/tmp/urnetwork-update-[A-Za-z0-9]{6}/update\.tar\.gz$'

# ============================================================================
# SECTION 1: Successful download on the first (primary) attempt
# ============================================================================
echo ""
echo "=== SECTION 1: Primary download succeeds ==="

test_primary_success() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-1"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-1/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Primary success: update exits 0"

    tarball_path="$(extract_tarball_paths "$CURL_LOG" | sort -u)"
    assert_eq "1" "$(printf '%s\n' "$tarball_path" | wc -l | tr -d ' ')" "Primary success: exactly one unique tarball path used"
    assert_matches "$tarball_path" "$TARBALL_PATTERN" "Primary success: tarball path matches busybox-safe dir/update.tar.gz pattern"
    assert_file_absent "$tarball_path" "Primary success: tarball removed after successful update"

    n_o_calls="$(grep -c -- ' -o ' "$CURL_LOG")"
    assert_eq "1" "$n_o_calls" "Primary success: mirror download was never attempted"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    if [ -x "$provider_bin" ]; then
        echo "  ✅ PASS: Primary success: provider binary installed and executable"
    else
        echo "  ❌ FAIL: Primary success: provider binary missing at $provider_bin"
        FAILS=$((FAILS + 1))
    fi

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Primary success: no leaked urnetwork-update-* artifacts under /tmp"
}
test_primary_success

# ============================================================================
# SECTION 2: Primary fails, GitHub mirror fallback succeeds
# ============================================================================
echo ""
echo "=== SECTION 2: Primary fails, mirror succeeds ==="

test_mirror_fallback_success() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-2"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-2/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=1
    MOCK_MIRROR_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Mirror fallback: update exits 0"

    n_o_calls="$(grep -c -- ' -o ' "$CURL_LOG")"
    assert_eq "2" "$n_o_calls" "Mirror fallback: both primary and mirror downloads were attempted"

    tarball_paths_unique="$(extract_tarball_paths "$CURL_LOG" | sort -u)"
    assert_eq "1" "$(printf '%s\n' "$tarball_paths_unique" | wc -l | tr -d ' ')" \
        "Mirror fallback: primary and mirror attempts reuse the same tarball path"
    assert_matches "$tarball_paths_unique" "$TARBALL_PATTERN" "Mirror fallback: reused tarball path matches mktemp pattern"
    assert_file_absent "$tarball_paths_unique" "Mirror fallback: tarball removed after successful update"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    if [ -x "$provider_bin" ]; then
        echo "  ✅ PASS: Mirror fallback: provider binary installed and executable"
    else
        echo "  ❌ FAIL: Mirror fallback: provider binary missing at $provider_bin"
        FAILS=$((FAILS + 1))
    fi

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Mirror fallback: no leaked urnetwork-update-* artifacts under /tmp"
}
test_mirror_fallback_success

# ============================================================================
# SECTION 3: Both primary and mirror downloads fail
# ============================================================================
echo ""
echo "=== SECTION 3: Both downloads fail ==="

test_both_downloads_fail() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-3"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-3/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=1
    MOCK_MIRROR_FAIL=1
    unset ec
    run_update

    assert_exit_code "1" "$ec" "Both downloads fail: update exits 1"

    n_o_calls="$(grep -c -- ' -o ' "$CURL_LOG")"
    assert_eq "2" "$n_o_calls" "Both downloads fail: both primary and mirror attempted"

    tarball_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"
    assert_matches "$tarball_path" "$TARBALL_PATTERN" "Both downloads fail: tarball path recorded matches mktemp pattern"
    assert_file_absent "$tarball_path" "Both downloads fail: tarball dir removed via 'rm -rf' cleanup on total failure"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Both downloads fail: no leaked urnetwork-update-* artifacts under /tmp"
}
test_both_downloads_fail

# ============================================================================
# SECTION 4: Download succeeds but tar extraction fails
# ============================================================================
echo ""
echo "=== SECTION 4: Tarball extraction fails ==="

test_extraction_fails() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-4"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-4/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    MOCK_TAR_FAIL=1
    unset ec
    run_update

    assert_exit_code "1" "$ec" "Extraction fails: update exits 1"

    tarball_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"
    assert_file_absent "$tarball_path" "Extraction fails: tarball removed alongside failed tmpdir"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Extraction fails: no leaked tarball or tmpdir under /tmp"
}
test_extraction_fails

# ============================================================================
# SECTION 5: Extraction succeeds but provider binary missing from tarball
# ============================================================================
echo ""
echo "=== SECTION 5: Provider binary missing from tarball ==="

test_provider_missing_in_tarball() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-5"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-5/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    MOCK_TAR_NO_PROVIDER=1
    unset ec
    run_update

    assert_exit_code "1" "$ec" "Missing binary: update exits 1"

    tarball_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"
    assert_file_absent "$tarball_path" "Missing binary: tarball removed"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    assert_file_absent "$provider_bin" "Missing binary: provider binary was never installed"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Missing binary: no leaked tarball or tmpdir under /tmp"
}
test_provider_missing_in_tarball

# ============================================================================
# SECTION 6: Regression guard - filename unpredictability
# ============================================================================
echo ""
echo "=== SECTION 6: Tarball filename is unique per run (not the old fixed name) ==="

test_tarball_name_is_unpredictable() {
    reset_fixture
    MOCK_VERSION="v9.9.9-mock-6a"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-6a/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=1
    MOCK_MIRROR_FAIL=1
    unset ec
    run_update
    first_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"

    reset_fixture
    MOCK_VERSION="v9.9.9-mock-6b"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-6b/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=1
    MOCK_MIRROR_FAIL=1
    unset ec
    run_update
    second_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"

    assert_matches "$first_path" "$TARBALL_PATTERN" "Unpredictability: first run's tarball path matches mktemp pattern"
    assert_matches "$second_path" "$TARBALL_PATTERN" "Unpredictability: second run's tarball path matches mktemp pattern"

    if [ "$first_path" != "$second_path" ]; then
        echo "  ✅ PASS: Unpredictability: two separate runs get distinct tarball filenames"
    else
        echo "  ❌ FAIL: Unpredictability: two separate runs reused the same tarball filename ($first_path)"
        FAILS=$((FAILS + 1))
    fi
}
test_tarball_name_is_unpredictable

# ============================================================================
# SECTION 7: idle-update with --window 0 — proceeds immediately
# ============================================================================
echo ""
echo "=== SECTION 7: idle-update --window 0 (no monitoring wait) ==="

run_idle_update() {
    out="$(
        MOCK_VERSION="${MOCK_VERSION:-}" \
        MOCK_DOWNLOAD_URL="${MOCK_DOWNLOAD_URL:-}" \
        MOCK_PRIMARY_FAIL="${MOCK_PRIMARY_FAIL:-0}" \
        MOCK_MIRROR_FAIL="${MOCK_MIRROR_FAIL:-0}" \
        MOCK_TAR_FAIL="${MOCK_TAR_FAIL:-0}" \
        MOCK_TAR_NO_PROVIDER="${MOCK_TAR_NO_PROVIDER:-0}" \
        MOCK_ARCH="${MOCK_ARCH:-x86_64}" \
        CURL_LOG="$CURL_LOG" \
        HOME="${MOCK_HOME:-$TEMP_DIR/home}" \
        PATH="$MOCKBIN:$PATH" \
        bash "$SCRIPT_COPY" idle-update --window 0 2>&1
    )" || ec=$?
    ec="${ec:-0}"
}

test_idle_update_window_zero() {
    reset_fixture
    MOCK_VERSION="v9.9.9-idle-1"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-idle-1/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_idle_update

    assert_exit_code "0" "$ec" "idle-update --window 0: exits 0"
    assert_matches "$out" "Waiting for billable traffic" "idle-update --window 0: shows monitoring header"
    assert_matches "$out" "quiet.*need 0s" "idle-update --window 0: reports need 0s"
    assert_matches "$out" "Provider binary updated" "idle-update --window 0: update completed"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    if [ -x "$provider_bin" ]; then
        echo "  ✅ PASS: idle-update --window 0: provider binary installed"
    else
        echo "  ❌ FAIL: idle-update --window 0: provider binary missing"
        FAILS=$((FAILS + 1))
    fi
}
test_idle_update_window_zero

# ============================================================================
# SECTION 8: idle-update with custom threshold and rate file
# ============================================================================
echo ""
echo "=== SECTION 8: idle-update with custom threshold and billable_rate file ==="

test_idle_update_custom_threshold() {
    reset_fixture

    HEALTH_DIR="$TEMP_DIR/health"
    mkdir -p "$HEALTH_DIR"
    printf '50\n' > "$HEALTH_DIR/billable_rate"

    MOCK_VERSION="v9.9.9-idle-2"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-idle-2/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    out="$(
        MOCK_VERSION="${MOCK_VERSION:-}" \
        MOCK_DOWNLOAD_URL="${MOCK_DOWNLOAD_URL:-}" \
        MOCK_PRIMARY_FAIL="${MOCK_PRIMARY_FAIL:-0}" \
        MOCK_MIRROR_FAIL="${MOCK_MIRROR_FAIL:-0}" \
        MOCK_TAR_FAIL="${MOCK_TAR_FAIL:-0}" \
        MOCK_TAR_NO_PROVIDER="${MOCK_TAR_NO_PROVIDER:-0}" \
        MOCK_ARCH="${MOCK_ARCH:-x86_64}" \
        CURL_LOG="$CURL_LOG" \
        URNETWORK_PROXY_HEALTH_DIR="$HEALTH_DIR" \
        HOME="${MOCK_HOME:-$TEMP_DIR/home}" \
        PATH="$MOCKBIN:$PATH" \
        bash "$SCRIPT_COPY" idle-update --threshold 100 --window 0 2>&1
    )" || ec=$?
    ec="${ec:-0}"

    assert_exit_code "0" "$ec" "idle-update custom threshold: exits 0"
    assert_matches "$out" "50 B/s.*quiet" "idle-update custom: reads billable_rate = 50 B/s"
    assert_matches "$out" "Provider binary updated" "idle-update custom: update completed"

    rm -rf "$HEALTH_DIR"
}
test_idle_update_custom_threshold

# ============================================================================
# SECTION 9: Zero-byte / truncated tarball download
# ============================================================================
echo ""
echo "=== SECTION 9: Zero-byte tarball fails extraction cleanly ==="

test_zero_byte_tarball_fails_cleanly() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-9"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-9/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    MOCK_TARBALL_CONTENT=""
    # A 0-byte/truncated archive is exactly what a real tar would reject;
    # the mock tar mirrors that outcome via MOCK_TAR_FAIL.
    MOCK_TAR_FAIL=1
    unset ec
    run_update

    assert_exit_code "1" "$ec" "Zero-byte tarball: update exits 1"
    assert_matches "$out" "failed to extract tarball" "Zero-byte tarball: reports extraction failure"

    tarball_path="$(extract_tarball_paths "$CURL_LOG" | sort -u | head -n1)"
    assert_file_absent "$tarball_path" "Zero-byte tarball: tarball dir removed on failure"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    assert_file_absent "$provider_bin" "Zero-byte tarball: provider binary was never installed"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Zero-byte tarball: no leaked artifacts under /tmp"
}
test_zero_byte_tarball_fails_cleanly

echo ""
echo "=== SECTION 9b: Partial primary download is overwritten by mirror ==="

test_partial_primary_overwritten_by_mirror() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-9b"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-9b/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=1
    MOCK_MIRROR_FAIL=0
    # Primary writes a short garbage prefix to the shared tarball path
    # before it fails; the mirror attempt must overwrite (not append to)
    # that same path and still produce a valid update.
    MOCK_PRIMARY_PARTIAL_CONTENT="garbage-partial-bytes-from-a-dropped-connection"
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Partial primary: update still succeeds via mirror overwrite"
    assert_matches "$out" "Primary download failed, trying GitHub mirror" "Partial primary: mirror fallback message shown"

    n_o_calls="$(grep -c -- ' -o ' "$CURL_LOG")"
    assert_eq "2" "$n_o_calls" "Partial primary: both primary and mirror attempted"

    tarball_paths_unique="$(extract_tarball_paths "$CURL_LOG" | sort -u)"
    assert_eq "1" "$(printf '%s\n' "$tarball_paths_unique" | wc -l | tr -d ' ')" \
        "Partial primary: mirror reused the same tarball path primary partially wrote"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    if [ -x "$provider_bin" ]; then
        echo "  ✅ PASS: Partial primary: provider binary installed from mirror content"
    else
        echo "  ❌ FAIL: Partial primary: provider binary missing at $provider_bin"
        FAILS=$((FAILS + 1))
    fi

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Partial primary: no leaked artifacts under /tmp"
}
test_partial_primary_overwritten_by_mirror

# ============================================================================
# SECTION 10: update-pending marker lifecycle
# ============================================================================
echo ""
echo "=== SECTION 10: update-pending marker is created on success ==="

test_marker_created_on_success() {
    reset_fixture

    MOCK_VERSION="v9.9.9-mock-10"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-10/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Marker on success: update exits 0"

    marker="$MOCK_HOME/.urnetwork/update-pending"
    if [ -f "$marker" ]; then
        echo "  ✅ PASS: Marker on success: update-pending marker present after successful update"
    else
        echo "  ❌ FAIL: Marker on success: update-pending marker missing at $marker"
        FAILS=$((FAILS + 1))
    fi
}
test_marker_created_on_success

echo ""
echo "=== SECTION 10b: update-pending marker is removed when the final mv fails ==="

test_marker_removed_on_mv_failure() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-10b"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-10b/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0

    # Force `mv -f "$staged_provider" "$provider_bin"` to fail by removing
    # write permission on the destination directory (non-root, so this is a
    # real permission denial, not a mocked one).
    chmod 555 "$APP_DIR"
    unset ec
    run_update
    chmod 755 "$APP_DIR"

    assert_exit_code "1" "$ec" "Marker on mv failure: update exits 1"

    marker="$MOCK_HOME/.urnetwork/update-pending"
    assert_file_absent "$marker" "Marker on mv failure: update-pending marker removed after failed install"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    assert_file_absent "$provider_bin" "Marker on mv failure: provider binary was never installed"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Marker on mv failure: staged tmpdir/staged_provider fully cleaned up, no /tmp leak"
}
test_marker_removed_on_mv_failure

echo ""
echo "=== SECTION 10c: update-pending marker respects a custom HOME ==="

test_marker_dir_respects_custom_home() {
    reset_fixture
    CUSTOM_HOME="$TEMP_DIR/custom-home-$$"
    rm -rf "$CUSTOM_HOME"
    mkdir -p "$CUSTOM_HOME"
    MOCK_HOME="$CUSTOM_HOME"

    MOCK_VERSION="v9.9.9-mock-10c"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-10c/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Custom HOME: update exits 0"

    marker="$CUSTOM_HOME/.urnetwork/update-pending"
    if [ -f "$marker" ]; then
        echo "  ✅ PASS: Custom HOME: marker written under the custom HOME's .urnetwork dir"
    else
        echo "  ❌ FAIL: Custom HOME: marker missing at $marker"
        FAILS=$((FAILS + 1))
    fi

    default_marker="$TEMP_DIR/home/.urnetwork/update-pending"
    assert_file_absent "$default_marker" "Custom HOME: default MOCK_HOME marker_dir left untouched"

    rm -rf "$CUSTOM_HOME"
}
test_marker_dir_respects_custom_home

# ============================================================================
# SECTION 11: Architecture mapping
# ============================================================================
echo ""
echo "=== SECTION 11: uname -m architecture mapping ==="

test_arch_x86_64_maps_to_amd64() {
    reset_fixture
    MOCK_ARCH=x86_64
    MOCK_VERSION="v9.9.9-mock-11a"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-11a/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Arch x86_64: update exits 0"
    if [ -x "$APP_DIR/urnetwork_amd64_stable" ]; then
        echo "  ✅ PASS: Arch x86_64: mapped to amd64 provider_bin path"
    else
        echo "  ❌ FAIL: Arch x86_64: expected $APP_DIR/urnetwork_amd64_stable to exist"
        FAILS=$((FAILS + 1))
    fi
    assert_file_absent "$APP_DIR/urnetwork_arm64_stable" "Arch x86_64: arm64 provider_bin path NOT used"
}
test_arch_x86_64_maps_to_amd64

test_arch_aarch64_maps_to_arm64() {
    reset_fixture
    MOCK_ARCH=aarch64
    MOCK_VERSION="v9.9.9-mock-11b"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-11b/urnetwork-linux-arm64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    run_update

    assert_exit_code "0" "$ec" "Arch aarch64: update exits 0"
    if [ -x "$APP_DIR/urnetwork_arm64_stable" ]; then
        echo "  ✅ PASS: Arch aarch64: mapped to arm64 provider_bin path"
    else
        echo "  ❌ FAIL: Arch aarch64: expected $APP_DIR/urnetwork_arm64_stable to exist"
        FAILS=$((FAILS + 1))
    fi
    assert_file_absent "$APP_DIR/urnetwork_amd64_stable" "Arch aarch64: amd64 provider_bin path NOT used"
}
test_arch_aarch64_maps_to_arm64

echo ""
echo "=== SECTION 11b: Unsupported architecture exits before any side effects ==="

test_arch_unsupported_exits_before_side_effects() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"
    MOCK_ARCH="riscv64"
    MOCK_VERSION="v9.9.9-mock-11c"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-11c/urnetwork-linux-riscv64.tar.gz"
    unset ec
    run_update

    assert_exit_code "1" "$ec" "Unsupported arch: update exits 1"
    assert_matches "$out" "unsupported architecture riscv64" "Unsupported arch: reports the offending arch"

    n_o_calls="$(grep -c -- ' -o ' "$CURL_LOG" || true)"
    assert_eq "0" "$n_o_calls" "Unsupported arch: no download attempted"
    assert_eq "0" "$(wc -l < "$CURL_LOG" | tr -d ' ')" "Unsupported arch: curl never invoked at all (fails before release check)"

    marker="$MOCK_HOME/.urnetwork/update-pending"
    assert_file_absent "$marker" "Unsupported arch: no update-pending marker written"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Unsupported arch: no /tmp artifacts created"
}
test_arch_unsupported_exits_before_side_effects

# ============================================================================
# SECTION 12: mktemp stub enforces busybox rules on the staged_provider path too
# ============================================================================
echo ""
echo "=== SECTION 12: busybox mktemp stub catches a staged_provider template regression ==="

test_mktemp_stub_catches_staged_provider_regression() {
    reset_fixture
    before="$(snapshot_tmp_artifacts)"

    MOCK_VERSION="v9.9.9-mock-12"
    MOCK_DOWNLOAD_URL="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/v9.9.9-mock-12/urnetwork-linux-amd64.tar.gz"
    MOCK_PRIMARY_FAIL=0
    unset ec
    RUN_SCRIPT="$REGRESSED_STAGED_SCRIPT" run_update
    unset RUN_SCRIPT

    assert_exit_code "1" "$ec" "Regressed staged_provider template: update exits 1"
    assert_matches "$out" "Invalid argument" "Regressed staged_provider template: mktemp stub rejects the .XXXXXX.new suffix"

    provider_bin="$APP_DIR/urnetwork_amd64_stable"
    assert_file_absent "$provider_bin" "Regressed staged_provider template: provider binary was never installed"

    after="$(snapshot_tmp_artifacts)"
    assert_eq "$before" "$after" "Regressed staged_provider template: tarball tmpdir still cleaned up despite the later failure"
}
test_mktemp_stub_catches_staged_provider_regression

# ============================================================================
echo ""
echo "=============================================="
if [ "$FAILS" -eq 0 ]; then
    echo "  All docker update tarball tests passed!"
    exit 0
else
    echo "  $FAILS test(s) failed"
    exit 1
fi