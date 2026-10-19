#!/usr/bin/env bash
# Run before pushing a release tag. A tag push starts build.yml, which runs
# gofmt and the full -short -race suite in shards; a failure there leaves a
# published pre-release without its Docker image. This runs the same checks
# locally, in the same shape, and stops at the first failure. It also builds
# the Docker image (amd64, no push) so a Dockerfile break cannot reach a tag; if docker is
# missing or not running that is a FAILURE unless --skip-docker is passed.
#
#   scripts/pre-tag-check.sh                  # everything, including the docker build
#   scripts/pre-tag-check.sh --quick          # gofmt, vet and build only (no tests, no docker build)
#   scripts/pre-tag-check.sh --skip-docker    # everything except the docker build (the run must say why)
set -euo pipefail
cd "$(dirname "$0")/.."

# Some tests bind a unix socket under t.TempDir(), whose path embeds the test
# name; a long TMPDIR pushes it past the 108 byte sun_path limit ("bind: invalid
# argument"). A hosted runner uses /tmp, so use /tmp here too, whatever the
# session set.
export TMPDIR=/tmp

quick=0
skip_docker=0
for arg in "$@"; do
  case "$arg" in
    --quick) quick=1 ;;
    --skip-docker) skip_docker=1 ;;
    *)
      echo "unknown option: $arg" >&2
      echo "usage: scripts/pre-tag-check.sh [--quick] [--skip-docker]" >&2
      exit 2
      ;;
  esac
done

echo "== gofmt (build.yml lint job)"
unformatted=$(gofmt -l . | grep -v "^third_party/" || true)
if [ -n "$unformatted" ]; then
  echo "Files not gofmt-clean:"
  echo "$unformatted"
  exit 1
fi

echo "== go vet, every release target"
for target in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 freebsd/amd64 freebsd/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} go vet ./... >/dev/null
  echo "  ok $target"
done

echo "== shard partition covers every package"
./.github/scripts/test_shard.sh --verify

if [ "$quick" = 1 ]; then
  echo "quick checks passed (tests and the docker build not run)"
  exit 0
fi

echo "== docker build (amd64, no push; build.yml build-and-push job)"
if [ "$skip_docker" = 1 ]; then
  echo "  SKIPPED via --skip-docker: build.yml still runs the Docker build at the tag;"
  echo "  the run/report must state why this check was skipped."
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker build --progress=plain -t urnetwork-pre-tag-check:local .
  echo "  docker build ok"
else
  echo "ERROR: docker is not installed or its daemon is not running, so the Dockerfile cannot be checked." >&2
  echo "  This is the class the rc4 image build failed on (run 38981852019)." >&2
  echo "  Install/start docker and re-run, or re-run with --skip-docker and state why it is skipped." >&2
  exit 1
fi

echo "== go test -short -race, one shard at a time (build.yml test job)"
for shard in 0 1 2; do
  pkgs=$(./.github/scripts/test_shard.sh "$shard")
  echo "-- shard $shard"
  # shellcheck disable=SC2086
  go test -short -race -timeout 900s $pkgs
done
# A GitHub runner differs from a workstation: unprivileged ICMP sockets are
# refused (net.ipv4.ping_group_range closed) and there is no global IPv6. A test
# that passes here can fail there. Run the root package once more in a network
# namespace that refuses ICMP sockets, with the race detector, like the runner.
echo "== root package in a network namespace that refuses ICMP sockets (-short -race)"
if command -v unshare >/dev/null 2>&1 && unshare -Urn true 2>/dev/null; then
  testbin=$(mktemp /tmp/connect-ci-like.XXXXXX)
  go test -c -race -o "$testbin" .
  # Only the ICMP and packet-parsing tests run here: the namespace has a
  # loopback and nothing else, which is what exposes ICMP state leaks but also
  # breaks tests that need a real network (the family and H3 tests).
  unshare -Urn sh -c "ip link set lo up 2>/dev/null; \"$testbin\" -test.short -test.timeout=900s -test.run \"Icmp|ParseIpPath|Ipv6|Smtp|Fragment|Packet|Flow|Dial|Parse\""
  rm -f "$testbin"
else
  echo "  unshare -Urn is not available here: skipping the ICMP-refused run (run it where it is)"
fi
echo "all pre-tag checks passed"
