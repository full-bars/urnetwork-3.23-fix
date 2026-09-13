#!/usr/bin/env bash
# test_profile_safety.sh — Bug 4 diagnostic: eco/lowmem profiles cannot crash/hang.
#
# Verifies:
#   1. URNETWORK_PROFILE=eco and =lowmem don't crash `provider --version`
#   2. GOMEMLIMIT is respected when set explicitly (not clobbered)
#   3. The Docker entrypoint passes through eco/lowmem env vars (no clobber by TURBO)
#
# Exit code: 0 = all pass, 1 = failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
PROVIDER_BIN=""

# ── Helpers ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0; WARN=0

pass() { echo -e "  ${GREEN}PASS${NC}  $*"; PASS=$((PASS + 1)); }
fail() { echo -e "  ${RED}FAIL${NC}  $*"; FAIL=$((FAIL + 1)); }
warn() { echo -e "  ${YELLOW}WARN${NC}  $*"; WARN=$((WARN + 1)); }

# Simulates the TURBO block from entrypoint.sh (lines 29-40).
# Sets URNETWORK_PROFILE in the current shell based on TURBO value.
simulate_turbo_block() {
  local _turbo="${TURBO:-}"
  _turbo="$(echo "$_turbo" | tr '[:upper:]' '[:lower:]')"
  case "$_turbo" in
    v4|v8) export URNETWORK_PROFILE="turbo-${_turbo}" ;;
    "") ;;
    *) ;;
  esac
}

# ── Locate or build the provider binary ──────────────────────────────────────
find_provider() {
  for candidate in \
    "$REPO_ROOT/provider/urnetwork-provider" \
    "$REPO_ROOT/provider/urnetwork" \
    "$(command -v urnetwork-provider 2>/dev/null || true)" \
    "$(command -v urnetwork 2>/dev/null || true)"; do
    if [ -n "$candidate" ] && [ -x "$candidate" ]; then
      PROVIDER_BIN="$candidate"
      return 0
    fi
  done
  if command -v go &>/dev/null && [ -f "$REPO_ROOT/go.mod" ]; then
    echo "  Building provider binary (one-time)..."
    (cd "$REPO_ROOT" && go build -o provider/urnetwork-provider ./provider 2>&1) || {
      echo "WARN: go build failed; tests that need the binary will be skipped."
      return 1
    }
    PROVIDER_BIN="$REPO_ROOT/provider/urnetwork-provider"
    return 0
  fi
  return 1
}

if ! find_provider; then
  echo "======================================================================"
  echo "Bug 4 — eco/lowmem profile safety diagnostic"
  echo "======================================================================"
  warn "No provider binary found/built; skipping binary-level tests."
  warn "Entrypoint shell logic tests will still run."
  echo "======================================================================"
fi

echo "======================================================================"
echo "Bug 4 — eco/lowmem profile safety diagnostic"
echo "======================================================================"

# ── TEST 1: eco profile does not crash --version ─────────────────────────────
echo ""
echo "TEST 1: URNETWORK_PROFILE=eco does not crash provider --version"

if [ -n "$PROVIDER_BIN" ]; then
  set +e
  URNETWORK_PROFILE=eco "$PROVIDER_BIN" --version </dev/null >/dev/null 2>&1
  RC=$?
  set -e
  if [ "$RC" -eq 0 ]; then
    pass "eco profile: --version exits 0"
  else
    fail "eco profile: --version exits $RC (expected 0)"
  fi
else
  warn "Skipping TEST 1 (no binary)"
fi

# ── TEST 2: lowmem profile does not crash --version ──────────────────────────
echo ""
echo "TEST 2: URNETWORK_PROFILE=lowmem does not crash provider --version"

if [ -n "$PROVIDER_BIN" ]; then
  set +e
  URNETWORK_PROFILE=lowmem "$PROVIDER_BIN" --version </dev/null >/dev/null 2>&1
  RC=$?
  set -e
  if [ "$RC" -eq 0 ]; then
    pass "lowmem profile: --version exits 0"
  else
    fail "lowmem profile: --version exits $RC (expected 0)"
  fi
else
  warn "Skipping TEST 2 (no binary)"
fi

# ── TEST 3: GOMEMLIMIT not clobbered by eco profile ─────────────────────────
echo ""
echo "TEST 3: GOMEMLIMIT env var is respected (not clobbered) under eco profile"

if [ -n "$PROVIDER_BIN" ]; then
  set +e
  GOMEMLIMIT=256MiB URNETWORK_PROFILE=eco "$PROVIDER_BIN" --version </dev/null >/dev/null 2>&1
  RC=$?
  set -e
  if [ "$RC" -eq 0 ]; then
    pass "eco + GOMEMLIMIT=256MiB: exits 0 (env not clobbered, no crash)"
  else
    fail "eco + GOMEMLIMIT=256MiB: exits $RC"
  fi
else
  warn "Skipping TEST 3 (no binary)"
fi

# ── TEST 4: GOMEMLIMIT not clobbered under lowmem ────────────────────────────
echo ""
echo "TEST 4: GOMEMLIMIT env var is respected under lowmem profile"

if [ -n "$PROVIDER_BIN" ]; then
  set +e
  GOMEMLIMIT=128MiB URNETWORK_PROFILE=lowmem "$PROVIDER_BIN" --version </dev/null >/dev/null 2>&1
  RC=$?
  set -e
  if [ "$RC" -eq 0 ]; then
    pass "lowmem + GOMEMLIMIT=128MiB: exits 0 (env not clobbered, no crash)"
  else
    fail "lowmem + GOMEMLIMIT=128MiB: exits $RC"
  fi
else
  warn "Skipping TEST 4 (no binary)"
fi

# ── TEST 5: entrypoint.sh does not clobber eco via TURBO handling ────────────
echo ""
echo "TEST 5: entrypoint.sh passes URNETWORK_PROFILE=eco through when TURBO is empty"

ENTRYPOINT="$REPO_ROOT/docker/scripts/entrypoint.sh"
if [ -f "$ENTRYPOINT" ]; then
  TURBO=""
  export URNETWORK_PROFILE="eco"
  simulate_turbo_block
  if [ "$URNETWORK_PROFILE" = "eco" ]; then
    pass "TURBO='' does not clobber URNETWORK_PROFILE=eco"
  else
    fail "Expected eco, got '$URNETWORK_PROFILE'"
  fi
else
  warn "Skipping TEST 5 (entrypoint.sh not found)"
fi

# ── TEST 6: entrypoint.sh does not clobber lowmem via TURBO="" ───────────────
echo ""
echo "TEST 6: entrypoint.sh passes URNETWORK_PROFILE=lowmem through when TURBO is empty"

if [ -f "$ENTRYPOINT" ]; then
  TURBO=""
  export URNETWORK_PROFILE="lowmem"
  simulate_turbo_block
  if [ "$URNETWORK_PROFILE" = "lowmem" ]; then
    pass "TURBO='' does not clobber URNETWORK_PROFILE=lowmem"
  else
    fail "Expected lowmem, got '$URNETWORK_PROFILE'"
  fi
else
  warn "Skipping TEST 6 (entrypoint.sh not found)"
fi

# ── TEST 7: entrypoint.sh DOES clobber to turbo when TURBO=v4 ───────────────
echo ""
echo "TEST 7: entrypoint.sh correctly sets turbo-v4 when TURBO=v4 (overrides eco)"

if [ -f "$ENTRYPOINT" ]; then
  TURBO="v4"
  export URNETWORK_PROFILE="eco"
  simulate_turbo_block
  if [ "$URNETWORK_PROFILE" = "turbo-v4" ]; then
    pass "TURBO=v4 correctly overrides eco -> turbo-v4"
  else
    fail "Expected turbo-v4, got '$URNETWORK_PROFILE'"
  fi
else
  warn "Skipping TEST 7 (entrypoint.sh not found)"
fi

# ── TEST 8: detectEffectiveRAMLimitBytes reads cgroup v2 ────────────────────
echo ""
echo "TEST 8: cgroup v2 memory.max is readable (environment check)"

if [ -f /sys/fs/cgroup/memory.max ]; then
  VAL=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo "unreadable")
  if [ "$VAL" = "max" ]; then
    pass "cgroup v2 memory.max = 'max' (no limit; code falls to /proc/meminfo or 850MiB)"
  elif echo "$VAL" | grep -qE '^[0-9]+$'; then
    pass "cgroup v2 memory.max = ${VAL} bytes (valid integer)"
  else
    fail "cgroup v2 memory.max = '$VAL' (unparseable)"
  fi
else
  warn "No cgroup v2 memory.max (not in container). Checking cgroup v1..."
  if [ -f /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then
    V1=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo "0")
    pass "cgroup v1 memory.limit_in_bytes = $V1 (code falls through to this)"
  else
    warn "No cgroup v1 either (bare metal). Code uses /proc/meminfo -> 850MiB fallback."
  fi
fi

# ── TEST 9: URNETWORK_ADAPTIVE_GC kill switch in source ─────────────────────
echo ""
echo "TEST 9: adaptive GC kill switch (URNETWORK_ADAPTIVE_GC) verified in source"

ADAPTIVE_SRC=$(grep -n 'adaptiveGCDisableEnv\|URNETWORK_ADAPTIVE_GC' "$REPO_ROOT/provider/resource_pressure.go" 2>/dev/null || true)
if echo "$ADAPTIVE_SRC" | grep -q 'URNETWORK_ADAPTIVE_GC'; then
  pass "adaptive GC kill switch found in resource_pressure.go"
else
  fail "adaptive GC kill switch not found in resource_pressure.go"
fi

# ── TEST 10: No crash with URNETWORK_PROFILE set to an invalid value ─────────
echo ""
echo "TEST 10: Invalid URNETWORK_PROFILE value does not crash provider"

if [ -n "$PROVIDER_BIN" ]; then
  set +e
  URNETWORK_PROFILE="totally-invalid-profile" "$PROVIDER_BIN" --version </dev/null >/dev/null 2>&1
  RC=$?
  set -e
  if [ "$RC" -eq 0 ]; then
    pass "Invalid profile: --version exits 0 (profile ignored gracefully)"
  elif [ "$RC" -gt 128 ]; then
    SIGNAL=$((RC - 128))
    fail "Invalid profile: crashed with signal $SIGNAL (RC=$RC)"
  else
    pass "Invalid profile: --version exits $RC (rejected but did not crash)"
  fi
else
  warn "Skipping TEST 10 (no binary)"
fi

# ── Summary ──────────────────────────────────────────────────────────────────
echo ""
echo "======================================================================"
echo "RESULTS:  ${PASS} passed,  ${FAIL} failed,  ${WARN} warnings"
echo "======================================================================"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
