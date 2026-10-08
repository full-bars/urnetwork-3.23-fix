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

load_rc_config $name

: ${%[1]s_enable:="NO"}
: ${%[1]s_user:="%[2]s"}
: ${%[1]s_flags:=""}

start_cmd="%[1]s_start"
stop_cmd="%[1]s_stop"
status_cmd="%[1]s_status"

# The provider is long-lived, so start it through daemon(8). Running it in the
# foreground would make "service %[1]s start" never return AND stall the boot
# sequence, because rc runs start synchronously.
#
# daemon(8) also settles two things a bare "su -m ... &" would not:
#   - -u sets HOME, USER and SHELL for the target user, so the provider's state
#     dir lands in that user's home rather than root's.
#   - -p writes the child pid through pidfile(3), giving stop and status a
#     precise handle on the provider process.
#
# No -m flag: daemon(8) defaults to sending BOTH stdout and stderr to the -o
# file. A mask of 0 (the previous value) plumbed neither, so the log stayed
# empty and the provider ran with its output discarded.
#
# rc.subr's own status/start pid logic is not used here: it keys off
# procname/command, and this script sets neither, so rc_pid would always be
# empty (start would launch duplicates, status would always report stopped).
# The pidfile is checked directly instead.
daemon="/usr/sbin/daemon"
pidfile="/var/run/%[1]s.pid"
logdir="/var/log/%[1]s"

%[1]s_running_pid()
{
    # Print the live provider pid, or nothing when it is not running.
    [ -f "$pidfile" ] || return 1
    _pid="$(cat "$pidfile" 2>/dev/null)"
    [ -n "$_pid" ] || return 1
    kill -0 "$_pid" 2>/dev/null || return 1
    echo "$_pid"
}

%[1]s_start()
{
    if _pid="$(${name}_running_pid)"; then
        echo "%[1]s is already running as pid $_pid; not starting a second copy."
        return 0
    fi
    rm -f "$pidfile"
    mkdir -p "$logdir"
    chown ${%[1]s_user} "$logdir"
    echo "Starting %[1]s."
    $daemon -f -t %[1]s -p "$pidfile" -o "$logdir/stdout.log" \
        -u ${%[1]s_user} \
        "%[4]s" provide $%[1]s_flags
}

%[1]s_stop()
{
    echo "Stopping %[1]s."
    if ! _pid="$(${name}_running_pid)"; then
        echo "%[1]s is not running."
        rm -f "$pidfile"
        return 0
    fi
    kill "$_pid" 2>/dev/null || true
    _n=0
    while [ "$_n" -lt 30 ]; do
        kill -0 "$_pid" 2>/dev/null || break
        sleep 1
        _n=$((_n + 1))
    done
    if kill -0 "$_pid" 2>/dev/null; then
        echo "%[1]s did not exit within 30s; sending SIGKILL."
        kill -9 "$_pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
}

%[1]s_status()
{
    if _pid="$(${name}_running_pid)"; then
        echo "%[1]s is running as pid $_pid."
        return 0
    fi
    echo "%[1]s is not running."
    return 1
}

run_rc_command "$1"
`, service, user, workingDir, binary)
}

// bsdRcScriptDir is where FreeBSD keeps rc.d service scripts.
const bsdRcScriptDir = "/usr/local/etc/rc.d"

// bsdRcScriptPath is the on-disk path of the rc.d script for a service name.
func bsdRcScriptPath(service string) string {
	return bsdRcScriptDir + "/" + service
}

// bsdRcServiceName is the rc.d service name for a provider binary. Derived from
// the binary so a beta build and a stable build on one box get different
// services instead of fighting over a single name.
func bsdRcServiceName(binary string) string {
	base := providerBinaryBasename(binary)
	if base == "" {
		return "urnetwork"
	}
	// rc.conf variables are SHELL variables: a hyphen, dot or space in the
	// name would make `<name>_enable` an invalid assignment. sysrc still
	// writes such a line and every boot then fails to source it, which is the
	// same silent-no-op this package already fixed for sysctl persistence. So
	// map anything outside [a-z0-9_] to _, the same way the kernel's rc
	// scripts sanitize service names.
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" {
		return "urnetwork"
	}
	// A shell variable name cannot start with a digit.
	if name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	return name
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
