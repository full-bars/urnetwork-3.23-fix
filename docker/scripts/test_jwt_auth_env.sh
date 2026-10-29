#!/bin/sh
# Pins the auth-code hand-off in provider_common.sh.
#
# The environment-auth branch used to call `auth "" -f`, passing an empty
# argument. The binary then fell back to its interactive prompt, which reads
# stdin, and in a container with no TTY that is EOF and a panic. The code has
# to be handed to the binary as the argument.
set -eu

script="$(dirname "$0")/provider_common.sh"
fail=0

if grep -q 'auth "" -f' "$script"; then
    echo "FAIL: $script calls auth with an empty argument; the code is not forwarded"
    grep -n 'auth "" -f' "$script"
    fail=1
else
    echo "PASS: no empty-argument auth call"
fi

forwarded=$(grep -c 'auth "\$URNETWORK_AUTH_CODE" -f' "$script" || true)
if [ "$forwarded" -ne 2 ]; then
    echo "FAIL: expected 2 call sites forwarding $URNETWORK_AUTH_CODE, found $forwarded"
    fail=1
else
    echo "PASS: both auth call sites forward the code"
fi

if ! sh -n "$script" 2>/dev/null; then
    echo "FAIL: $script does not parse"
    fail=1
else
    echo "PASS: $script parses"
fi

[ "$fail" -eq 0 ] || { echo "Results: FAILED"; exit 1; }
echo "Results: all passed"
