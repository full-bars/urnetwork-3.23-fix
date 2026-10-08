package urnettools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// renderBSDServiceScript returns the rc.d script for a provider. It is a pure
// function of its inputs so the script shape can be asserted in a test without
// writing to /usr/local/etc.
//
// rc.conf carries the overrides (<service>_enable, _user, _flags) because that
// is the rc.subr convention: editing rc.conf is how an operator changes the
// service's settings.
func renderBSDServiceScript(service, binary, user, workingDir string) string {
	return fmt.Sprintf(`#!/bin/sh
#
# PROVIDE: %[1]s
# REQUIRE: LOGIN NETWORKING
# KEYWORD: shutdown
#
# rc.conf knobs:
#   %[1]s_enable="YES"   start at boot (set by urnet-tools auto-start)
#   %[1]s_user="%[2]s"   run as this user
#   %[1]s_flags=""       extra flags for the provider
#
# To run at boot:
#   sysrc %[1]s_enable=YES

. /etc/rc.subr

name="%[1]s"
rcvar=%[1]s_enable

: $%[1]s_enable="NO"
: $%[1]s_user="%[2]s"
: $%[1]s_flags=""

load_rc_config $name

: ${%[1]s_enable:="NO"}
: ${%[1]s_user:="%[2]s"}
: ${%[1]s_flags:=""}

start_cmd="%[1]s_start"
stop_cmd="%[1]s_stop"

# The provider is long-lived, so start it through daemon(8). Running it in the
# foreground would make "service %[1]s start" never return AND stall the boot
# sequence, because rc runs start synchronously.
#
# daemon(8) also settles two things a bare "su -m ... &" would not:
#   - -u sets HOME, USER and SHELL for the target user, so the provider's state
#     dir lands in that user's home rather than root's.
#   - -p writes the child pid through pidfile(3), which is what gives
#     "service %[1]s onestatus" something real to check. Without it status
#     always reports "not running" and every start launches a duplicate.
daemon="/usr/sbin/daemon"
pidfile="/var/run/%[1]s.pid"
logdir="/var/log/%[1]s"

%[1]s_start()
{
    mkdir -p "$logdir"
    chown ${%[1]s_user} "$logdir"
    echo "Starting %[1]s."
    $daemon -f -t %[1]s -p "$pidfile" -o "$logdir/stdout.log" -m 0 \
        -u ${%[1]s_user} \
        "%[4]s" provide $%[1]s_flags
}

%[1]s_stop()
{
    echo "Stopping %[1]s."
    # Signal the CHILD, whose pid the pidfile holds. A "-f" pattern on the
    # binary path would also match urnet-tools, cron and anything else that
    # merely mentions the path, so the pidfile is the precise handle.
    if [ -f "$pidfile" ]; then
        pid="$(cat "$pidfile")"
        kill "$pid" 2>/dev/null || true
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            [ -f "$pidfile" ] || break
            sleep 1
        done
    fi
    pkill -TERM -x "$(basename "%[4]s")" 2>/dev/null || true
    rm -f "$pidfile"
}
run_rc_command "$1"
`, service, user, workingDir, binary)
}

// bsdRcServiceName is the rc.d service name for a provider binary. Derived from
// the binary so a beta build and a stable build on one box get different
// services instead of fighting over a single name.
func bsdRcServiceName(binary string) string {
	base := providerBinaryBasename(binary)
	if base == "" {
		return "urnetwork"
	}
	return base
}

// providerBinaryBasename is the lowercase basename of a provider binary, used
// to name the rc.d service after the binary it runs.
func providerBinaryBasename(binary string) string {
	base := strings.ToLower(filepath.Base(binary))
	// filepath.Base("") is ".", which would name the service "." — refuse it
	// rather than produce a path-shaped service name.
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}
