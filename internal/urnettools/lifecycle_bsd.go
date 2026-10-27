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
    _pf_pid=""
    if [ -f "$pidfile" ] && [ -r "$pidfile" ]; then
        _pf_pid="$(cat "$pidfile" 2>/dev/null)"
        _pid="$_pf_pid"
        if [ -n "$_pid" ] && ps -p "$_pid" -o args= 2>/dev/null | awk -v b='%[4]s' '$1 == b && $2 == "provide" {found=1} END {exit !found}'; then
            echo "$_pid"
            return 0
        fi
    fi
    _pid="$(ps -axo pid=,args= 2>/dev/null | awk -v b='%[4]s' '$2 == b && $3 == "provide" {print $1; exit}')"
    if [ -n "$_pid" ]; then
        echo "$_pid"
        return 0
    fi
    # daemon(8) writes the child pid before the child has exec'd the provider,
    # so an alive pidfile pid that does not identify yet can still be this
    # service starting, not a stale file. Accept it only while its argv is
    # still daemon(8)'s command line FOR THIS SERVICE: the -t title and the
    # provider command must both be there. A pid reused by an unrelated
    # process has neither, so it no longer makes start report "already
    # running" and skip a real start.
    _pargs="$(ps -p "$_pf_pid" -o args= 2>/dev/null)"
    case "$_pargs" in
        "$daemon"*" -t %[1]s "*"%[4]s provide"*) echo "$_pf_pid"; return 0 ;;
    esac
    return 1
}

%[1]s_start()
{
    if _pid="$(${name}_running_pid)"; then
        echo "%[1]s is already running as pid $_pid; not starting a second copy."
        return 0
    fi
    rm -f "$pidfile"
    mkdir -p "$logdir"
    chown root:wheel "$logdir" 2>/dev/null || true
    chmod 0755 "$logdir" 2>/dev/null || true
    if [ -L "$logdir/stdout.log" ]; then
        echo "Refusing to start: $logdir/stdout.log is a symlink (privilege escalation risk)."
        return 1
    fi
    # daemon(8) opens this path as root, so an existing log is kept only while
    # it is a plain file with a single link that the service user owns. A fifo,
    # a hard link or a foreign owner is replaced: removing the name leaves the
    # inode it pointed at alone.
    if [ -e "$logdir/stdout.log" ] && [ "$(stat -f '%%HT %%l %%Su' "$logdir/stdout.log" 2>/dev/null)" != "Regular File 1 ${%[1]s_user}" ]; then
        rm -f "$logdir/stdout.log" || return 1
    fi
    # Create the log only when it is missing: installing over an existing
    # file truncates it, so every restart would erase the previous run.
    if [ ! -e "$logdir/stdout.log" ]; then
        install -o ${%[1]s_user} -m 640 /dev/null "$logdir/stdout.log" 2>/dev/null || return 1
    fi
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
