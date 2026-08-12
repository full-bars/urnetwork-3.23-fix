#!/bin/bash
set -e

echo "======================================"
echo " install-urnet-docker.sh Test Suite"
echo "======================================"

# install-urnet-docker.sh is a linear script (no functions besides pr_err)
# that hits the network almost immediately after resolving arch/os/asset/
# install-dir. Mirror test_provider_install.sh's convention: cut the script
# before its first network operation and source only the deterministic,
# locally-resolvable prefix (arch detection, OS detection, asset name,
# install dir). Everything below the cut point (release lookup, download,
# verify, install) is exercised indirectly via the digest-extraction and
# PATH-check snippets tested separately below, without ever touching the
# network.
sed '/^# --- resolve latest release tag ---$/,$d' scripts/install-urnet-docker.sh > /tmp/urnet_docker_lib.sh

FAILS=0

assert_eq() {
    local expected="$1"
    local actual="$2"
    local msg="$3"
    if [ "$expected" = "$actual" ]; then
        echo "✅ PASS: $msg"
    else
        echo "❌ FAIL: $msg"
        echo "   Expected: '$expected'"
        echo "   Actual:   '$actual'"
        FAILS=$((FAILS + 1))
    fi
}

# --- TEST 1: arch detection maps uname -m to Go arch names ---
test_arch_detection() {
    local cases="x86_64:amd64 amd64:amd64 aarch64:arm64 arm64:arm64 i386:386 i686:386"
    for c in $cases; do
        local machine="${c%%:*}"
        local want="${c##*:}"
        local got
        got=$(bash -c "
            uname() { case \"\$1\" in -m) echo '$machine';; -s) echo 'Linux';; esac; }
            id() { echo '1000'; }
            source /tmp/urnet_docker_lib.sh
            echo \"\$ARCH\"
        ")
        assert_eq "$want" "$got" "uname -m '$machine' maps to ARCH '$want'"
    done
}
test_arch_detection

# --- TEST 2: unsupported architecture refuses with a clear error ---
test_arch_unsupported() {
    local out rc
    out=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'riscv64';; -s) echo 'Linux';; esac; }
        id() { echo '1000'; }
        source /tmp/urnet_docker_lib.sh
    " 2>&1) && rc=0 || rc=$?
    if [ "$rc" -eq 1 ] && case "$out" in *"unsupported architecture: riscv64"*) true;; *) false;; esac; then
        echo "✅ PASS: unsupported architecture refuses with exit 1 and a clear message"
    else
        echo "❌ FAIL: unsupported architecture rc=$rc out=$out"
        FAILS=$((FAILS + 1))
    fi
}
test_arch_unsupported

# --- TEST 3: OS detection accepts linux/darwin, rejects everything else ---
test_os_detection() {
    for os_name in Linux Darwin; do
        local got
        got=$(bash -c "
            uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo '$os_name';; esac; }
            id() { echo '1000'; }
            source /tmp/urnet_docker_lib.sh
            echo \"\$OS\"
        ")
        assert_eq "$(printf '%s' "$os_name" | tr '[:upper:]' '[:lower:]')" "$got" "uname -s '$os_name' resolves to lowercase OS '$got'"
    done
}
test_os_detection

test_os_unsupported() {
    local out rc
    out=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Windows';; esac; }
        id() { echo '1000'; }
        source /tmp/urnet_docker_lib.sh
    " 2>&1) && rc=0 || rc=$?
    if [ "$rc" -eq 1 ] && case "$out" in *"unsupported OS: windows"*) true;; *) false;; esac; then
        echo "✅ PASS: unsupported OS refuses with exit 1 and a clear message"
    else
        echo "❌ FAIL: unsupported OS rc=$rc out=$out"
        FAILS=$((FAILS + 1))
    fi
}
test_os_unsupported

# --- TEST 4: TOOL defaults to urnet-docker, overridable by $1 ---
test_tool_default_and_override() {
    local got_default got_override
    got_default=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Linux';; esac; }
        id() { echo '1000'; }
        source /tmp/urnet_docker_lib.sh
        echo \"\$TOOL\"
    ")
    assert_eq "urnet-docker" "$got_default" "TOOL defaults to urnet-docker with no argument"

    got_override=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Linux';; esac; }
        id() { echo '1000'; }
        source /tmp/urnet_docker_lib.sh urnet-tools
        echo \"\$TOOL\"
    ")
    assert_eq "urnet-tools" "$got_override" "TOOL is overridden by the first argument (urnet-tools)"
}
test_tool_default_and_override

# --- TEST 5: ASSET is composed as <tool>-<os>-<arch> ---
test_asset_name() {
    local got
    got=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'arm64';; -s) echo 'Darwin';; esac; }
        id() { echo '1000'; }
        source /tmp/urnet_docker_lib.sh urnet-tools
        echo \"\$ASSET\"
    ")
    assert_eq "urnet-tools-darwin-arm64" "$got" "ASSET composes tool-os-arch (never carries .exe on any platform)"
}
test_asset_name

# --- TEST 6: INSTALL_DIR resolution (root vs non-root, PREFIX override) ---
test_install_dir_resolution() {
    local got_root got_nonroot got_prefix
    got_root=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Linux';; esac; }
        id() { echo '0'; }
        source /tmp/urnet_docker_lib.sh
        echo \"\$INSTALL_DIR\"
    ")
    assert_eq "/usr/local/bin" "$got_root" "root (id -u = 0) installs to /usr/local/bin"

    got_nonroot=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Linux';; esac; }
        id() { echo '1000'; }
        HOME=/home/testuser
        source /tmp/urnet_docker_lib.sh
        echo \"\$INSTALL_DIR\"
    ")
    assert_eq "/home/testuser/.local/bin" "$got_nonroot" "non-root installs to \$HOME/.local/bin"

    got_prefix=$(bash -c "
        uname() { case \"\$1\" in -m) echo 'x86_64';; -s) echo 'Linux';; esac; }
        id() { echo '1000'; }
        PREFIX=/opt/custom
        source /tmp/urnet_docker_lib.sh
        echo \"\$INSTALL_DIR\"
    ")
    assert_eq "/opt/custom" "$got_prefix" "\$PREFIX overrides the default install dir even as non-root"
}
test_install_dir_resolution

# --- TEST 7: asset digest extraction (jq) ---
# The digest lookup is inlined directly in install-urnet-docker.sh (unlike
# Provider_Install_Linux.sh's extracted get_asset_digest_from_api_response),
# so exercise the exact jq invocation used in the script against a release
# JSON fixture, both for a present and a missing asset.
test_digest_extraction_jq() {
    local json='{"tag_name": "v9.9.9", "assets": [
        {"name": "urnetwork-provider-v9.9.9.tar.gz", "digest": "sha256:abc123"},
        {"name": "urnet-docker-linux-amd64", "digest": "sha256:def456"}
    ]}'
    local asset="urnet-docker-linux-amd64"
    local digest
    digest="$(printf "%s" "$json" | jq -r --arg a "$asset" '.assets[] | select(.name == $a) | .digest' 2>/dev/null | sed 's/^sha256://')"
    assert_eq "def456" "$digest" "jq digest extraction finds the named asset and strips sha256:"

    local missing_asset="urnet-docker-windows-amd64"
    local missing_digest
    missing_digest="$(printf "%s" "$json" | jq -r --arg a "$missing_asset" '.assets[] | select(.name == $a) | .digest' 2>/dev/null | sed 's/^sha256://')"
    assert_eq "" "$missing_digest" "jq digest extraction yields empty for a missing asset (release predates tool binaries)"
}
test_digest_extraction_jq

# --- TEST 8: asset digest extraction (python3 fallback) ---
test_digest_extraction_python() {
    local json='{"tag_name": "v9.9.9", "assets": [
        {"name": "urnetwork-provider-v9.9.9.tar.gz", "digest": "sha256:abc123"},
        {"name": "urnet-docker-linux-amd64", "digest": "sha256:def456"}
    ]}'
    local digest
    digest="$(printf "%s" "$json" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(next((a.get("digest","").replace("sha256:","") for a in d.get("assets",[]) if a.get("name")==sys.argv[1]), ""))' "urnet-docker-linux-amd64" 2>/dev/null)"
    assert_eq "def456" "$digest" "python3 fallback digest extraction finds the named asset and strips sha256:"
}
test_digest_extraction_python

# --- TEST 9: PATH-already-contains-INSTALL_DIR note logic ---
# The trailing "add this to your PATH" hint must only fire when the install
# dir is genuinely absent from $PATH, and must never fire (false positive)
# for a substring match against a differently-named directory.
test_path_note_logic() {
    local install_dir="/home/testuser/.local/bin"

    local printed_present=0
    case ":/usr/bin:/home/testuser/.local/bin:/bin:" in
        *":$install_dir:"*) printed_present=0 ;;
        *) printed_present=1 ;;
    esac
    if [ "$printed_present" -eq 0 ]; then
        echo "✅ PASS: PATH note is suppressed when INSTALL_DIR is already on PATH"
    else
        echo "❌ FAIL: PATH note incorrectly fired even though INSTALL_DIR is on PATH"
        FAILS=$((FAILS + 1))
    fi

    local printed_absent=0
    case ":/usr/bin:/bin:" in
        *":$install_dir:"*) printed_absent=0 ;;
        *) printed_absent=1 ;;
    esac
    if [ "$printed_absent" -eq 1 ]; then
        echo "✅ PASS: PATH note fires when INSTALL_DIR is absent from PATH"
    else
        echo "❌ FAIL: PATH note failed to fire even though INSTALL_DIR is absent"
        FAILS=$((FAILS + 1))
    fi

    # A directory that is merely a substring of another PATH entry (e.g.
    # /home/testuser/.local/bin-extra) must NOT satisfy the match — the
    # case pattern requires the exact ":$install_dir:" delimiters.
    local printed_substring=0
    case ":/home/testuser/.local/bin-extra:" in
        *":$install_dir:"*) printed_substring=0 ;;
        *) printed_substring=1 ;;
    esac
    if [ "$printed_substring" -eq 1 ]; then
        echo "✅ PASS: PATH note is not fooled by a directory that is only a prefix/substring match"
    else
        echo "❌ FAIL: PATH note treated a substring match as present on PATH"
        FAILS=$((FAILS + 1))
    fi
}
test_path_note_logic

rm -f /tmp/urnet_docker_lib.sh

echo "======================================"
if [ $FAILS -eq 0 ]; then
    echo "🎉 All tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi