#!/bin/sh
# urnet-tools: URnetwork provider manager (FreeBSD)
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# https://github.com/full-bars/urnetwork-3.23-fix

me="$(basename "$0")"

show_help() {
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  install [<version>]      Download and install the provider + rc.d service"
    echo "  start                     Start the provider"
    echo "  stop                      Stop the provider"
    echo "  restart                   Restart the provider"
    echo "  status                    Show provider service status"
    echo "  update [<version>]        Upgrade to latest (or specified version)"
    echo "  version                   Show installed version"
    echo ""
    echo "Performance & Tuning:"
    echo "  hot-restart <on|off>      Reuse client JWT identities across restarts"
    echo "  self-heal [on|off]        Auto-regulate proxies (load gate + cleanup) (default: off)"
    echo ""
    echo "Session:"
    echo "  session save <file>       Export identity+proxy state (encrypted)"
    echo "  session load <file>       Import identity+proxy state, then restart"
    echo ""
    echo "Proxy Management:"
    echo "  proxy refresh             Re-read configs and hot-reload proxies"
    echo "  proxy remove-dead         Interactively prune dead/degraded/failing"
    echo "  proxy summary             Fleet summary (sources, health, counts)"
    echo ""
    echo "Options:"
    echo "  -h, --help                Show this help"
    echo "  -v, --version             Show version"
    echo "  -f, --force               Skip confirmation prompts"
    echo ""
    echo "https://github.com/full-bars/urnetwork-3.23-fix"
}

# --- helpers ---

pr_err() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

pr_info() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@"
}

pr_warn() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

# http_fetch <url> [output-file]
#
# One downloader for every fetch the installer makes. FreeBSD ships fetch(1) in
# the base system and curl only as a package, so requiring curl meant a clean
# host could not install at all - and the fix is not to document a package, it
# is to use what the base system has. curl first (it is what the docs install
# anyway), then fetch, then wget.
#
# Failure semantics are preserved rather than approximated: curl -f and fetch
# both exit nonzero on an HTTP error status, so a caller that treats a nonzero
# status as "no such asset" keeps working. libfetch sends every status other
# than 200/206/304, a redirect or an auth challenge to its error path
# (libfetch/http.c, the default case around the response switch), which is what
# makes fetch a faithful stand-in for curl -f and not just for curl.
#
# Writes to stdout when no output file is given, which is what the JSON and tag
# reads need.
http_fetch() {
    _url="$1"
    _out="${2:-}"
    if command -v curl >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            curl -fsSL "$_url" -o "$_out"
        else
            curl -fsSL "$_url"
        fi
        return $?
    fi
    if command -v fetch >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            fetch -q -o "$_out" "$_url"
        else
            fetch -q -o - "$_url"
        fi
        return $?
    fi
    if command -v wget >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            wget -q -O "$_out" "$_url"
        else
            wget -q -O - "$_url"
        fi
        return $?
    fi
    return 1
}

# --- service account resolution ---
#
# resolve_service_user prints the account the provider must run as. Order:
# an explicit override, then SUDO_USER (who actually invoked sudo; $USER is
# "root" there), then logname (the login session owner, which survives a
# plain `su -`), then the current uid. $USER alone is NOT trusted: it is
# unset under cron and is "root" for a plain root login.
resolve_service_user() {
    if [ -n "${SERVICE_USER_OVERRIDE:-}" ]; then
        printf '%s\n' "$SERVICE_USER_OVERRIDE"
        return
    fi
    if [ -n "${URNETWORK_USER:-}" ]; then
        printf '%s\n' "$URNETWORK_USER"
        return
    fi
    if id -u urnetwork >/dev/null 2>&1; then
        printf '%s\n' "urnetwork"
        return
    fi
    if [ -n "${SUDO_USER:-}" ]; then
        printf '%s\n' "$SUDO_USER"
        return
    fi
    _lu="$(logname 2>/dev/null || true)"
    if [ -n "$_lu" ] && [ "$_lu" != "root" ]; then
        printf '%s\n' "$_lu"
        return
    fi
    id -un
}

# user_home prints a user's home directory from the passwd database. $HOME is
# not usable when running under sudo (it may still be root's), and the service
# user must be able to read the install and state directories.
user_home() {
    _h="$(getent passwd "$1" 2>/dev/null | cut -d: -f6)"
    if [ -z "$_h" ]; then
        _h="$(pw usershow "$1" 2>/dev/null | cut -d: -f9)"
    fi
    printf '%s\n' "${_h:-$HOME}"
}

# user_shell prints the login shell of a user from the passwd database. The
# invoking user's $SHELL is not the service user's shell, and writing a bash
# fragment into a tcsh rc file is worse than not writing one.
user_shell() {
    _s="$(getent passwd "$1" 2>/dev/null | cut -d: -f7)"
    [ -n "$_s" ] || _s="$(pw usershow "$1" 2>/dev/null | cut -d: -f10)"
    printf '%s\n' "${_s:-/bin/sh}"
}

# --- paths ---
#
# The binary lives in the SAME location the Go tool's discovery fallback looks
# (bsdProviderCandidates in internal/urnettools/discover_freebsd.go), so a
# stopped install is still discoverable and lifecycle commands have a target.
# Keep the two in step.

service_name="urnetwork"
rc_script="/usr/local/etc/rc.d/$service_name"

# Paths: the install tree is root-owned at /usr/local/libexec/urnetwork-provider
# (with chmod go-w), while the state directory (~/.urnetwork) lives in the
# service user's home.
service_user="$(resolve_service_user)"
service_home="$(user_home "$service_user")"
install_path="/usr/local/libexec/urnetwork-provider"
provider_bin="$install_path/bin/urnetwork"
state_dir="$service_home/.urnetwork"
github_api="https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

# --- rc.d ---

# priv runs a command as root, through sudo when we are not already root.
# rc.d start/stop need it (the script's own mkdir/chown and `daemon -u`
# require root), and a non-root run that fails silently is how an install
# could report success with nothing running.
priv() {
    if [ "$(id -u)" = "0" ]; then
        "$@"
    elif command -v sudo > /dev/null 2>&1; then
        sudo "$@"
    else
        pr_err "root is required to run: %s" "$*"
        pr_err "install sudo (pkg install sudo) or re-run as root"
        return 1
    fi
}

# --- staged-file cleanup ---
#
# The staged binary and the staged version file live in the root-owned install
# tree, so they can only be removed with the privilege that created them: a
# plain `rm -f` after `priv cp` fails with EPERM and leaves a root-owned
# .urnetwork.new.<pid> behind on every failed non-root install. Every removal
# of a staged path goes through priv_rm.
#
# install_cleanup is the EXIT trap for the paths that leave without reaching an
# explicit cleanup branch (an interrupt, a failure further down). It must never
# make the failure worse, so it preserves the status it inherited, warns rather
# than failing when a removal does not work, and reads stdin from /dev/null so
# a sudo that wants a password fails at once instead of blocking the exit on a
# prompt nobody is watching.
staged_bin=""
staged_tool=""
staged_version=""

priv_rm() {
    if [ -z "${1:-}" ]; then
        return 0
    fi
    # -e and -L, not -f: a staged path that was never created is not an error,
    # and a symlink planted at the name must still be removed as a link.
    if [ ! -e "$1" ] && [ ! -L "$1" ]; then
        return 0
    fi
    priv rm -f "$1" < /dev/null 2>/dev/null ||
        pr_warn "could not remove staged file %s; remove it by hand" "$1"
    return 0
}

install_cleanup() {
    _status=$?
    trap - EXIT INT TERM HUP
    priv_rm "$staged_bin"
    priv_rm "$staged_tool"
    priv_rm "$staged_version"
    # mktemp -d ran as the invoking user, so this removal needs no privilege.
    if [ -n "${tmpdir:-}" ]; then
        rm -rf "$tmpdir" 2>/dev/null || true
    fi
    exit "$_status"
}

load_service() {
    priv service "$service_name" start
}

unload_service() {
    priv service "$service_name" stop
}

restart_service() {
    priv service "$service_name" restart
}

service_running() {
    # The script must exist first, or `service` errors on a fresh box.
    [ -x "/usr/local/etc/rc.d/$service_name" ] || return 1
    # Through priv: the rc.d status check runs `kill -0` on the provider pid,
    # and for an invoking user whose uid differs from the service user that
    # returns EPERM, so an unprivileged status would report "not running" while
    # the provider is up. On update that would skip the restart and leave the
    # old binary running.
    priv service "$service_name" onestatus >/dev/null 2>&1
}

# set_rc_conf_default KEY VALUE sets an rc.conf variable only when it is not
# already set. sysrc is the supported editor for rc.conf (atomic, preserves
# formatting). Setting unconditionally would clobber an operator's explicit
# choice on every update: an operator who disabled auto-start would find it
# re-enabled, and a custom _user would be overwritten.
set_rc_conf_default() {
    _key="$1"
    _val="$2"
    _cur="$(sysrc -n "$_key" 2>/dev/null || true)"
    if [ -z "$_cur" ]; then
        priv sysrc "$_key=$_val"
    fi
}

# --- rc.d script ---
#
# One template, emitted on stdout by write_rc_script_body so the root and
# non-root paths in do_install cannot drift apart. write_rc_script just wraps it
# into the file.

write_rc_script() {
    write_rc_script_body "${1:-$(resolve_service_user)}" > "$rc_script"
    chmod 755 "$rc_script"
}

# write_rc_script_body [user] emits the rc.d script on stdout so it can be piped
# to `sudo tee` without a second copy of the template drifting.
#
# $1 is the account the provider runs as. It defaults to the invoking user, but
# the caller must pass the resolved SUDO_USER-derived name: under sudo, $USER is
# commonly "root", and a service that runs the internet-facing relay as root is
# exactly what this must not produce.
#
# Paths are quoted in every command that interpolates them, because an install
# under a home directory containing a space would otherwise break `cd` and
# `su -c` at boot.
write_rc_script_body() {
    run_user="${1:-$(resolve_service_user)}"
    cat <<RCSCRIPT
#!/bin/sh
#
# PROVIDE: $service_name
# REQUIRE: LOGIN NETWORKING
# KEYWORD: shutdown
#
# rc.conf knobs:
#   ${service_name}_enable="YES"   start at boot (set by urnet-tools auto-start)
#   ${service_name}_user="$run_user"   run as this user
#   ${service_name}_flags=""       extra flags for the provider
#
# To run at boot:
#   sysrc ${service_name}_enable=YES

. /etc/rc.subr

name="$service_name"
rcvar=${service_name}_enable

load_rc_config \$name

: \${${service_name}_enable:="NO"}
: \${${service_name}_user:="$run_user"}
: \${${service_name}_flags:=""}

start_cmd="${service_name}_start"
stop_cmd="${service_name}_stop"
status_cmd="${service_name}_status"

# The provider is long-lived, so start it through daemon(8). Running it in the
# foreground would make "service $service_name start" never return AND stall the
# boot sequence, because rc runs start synchronously.
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
pidfile="/var/run/$service_name.pid"
logdir="/var/log/$service_name"

${service_name}_running_pid()
{
    # Print the live provider pid, or nothing when it is not running.
    # daemon(8) opens the pidfile with a hard-coded mode 0600 (pidfile_open
    # in daemon.c, not configurable through -p/-P) and start runs as root, so
    # an unprivileged one*status cannot read the file at all. When it is
    # unreadable, fall back to scanning the process table.
    _pf_pid=""
    if [ -f "\$pidfile" ] && [ -r "\$pidfile" ]; then
        _pf_pid="\$(cat "\$pidfile" 2>/dev/null)"
        _pid="\$_pf_pid"
        # daemon(8) writes the provider's pid, so a readable pidfile is only
        # trusted once that pid is alive AND is this provider running as the
        # service user. A stale file whose pid was reused would otherwise make
        # start skip a real start, and the two providers would share state the
        # provider assumes it owns exclusively.
        if [ -n "\$_pid" ] && ps -p "\$_pid" -o args= 2>/dev/null | awk -v b='$provider_bin' '\$1 == b && \$2 == "provide" {found=1} END {exit !found}'; then
            echo "\$_pid"
            return 0
        fi
        # stale or foreign pid: fall through to the scan below
    fi
    # -U selects processes by username; -u would only select the output
    # format. The match is done in awk on the argv fields on purpose: a
    # "grep -F '<binary> provide'" in this same pipeline carries the search
    # string in its own argv, and "ps -U <service user>" lists that grep when
    # the caller is the service user. The grep then matches itself and a
    # stopped provider reports as running (and start skips a real start).
    _pid="\$(ps -U "\${${service_name}_user}" -o pid=,args= 2>/dev/null | awk -v b='$provider_bin' '\$2 == b && \$3 == "provide" {print \$1; exit}')"
    if [ -n "\$_pid" ]; then
        echo "\$_pid"
        return 0
    fi
    # daemon(8) writes the child pid before the child has exec'd the provider,
    # so an alive pidfile pid that does not identify yet can still be this
    # service starting, not a stale file. Accept it only while its argv is
    # still daemon(8)'s command line FOR THIS SERVICE: the -t title and the
    # provider command must both be there. A pid reused by an unrelated
    # process has neither, so it no longer makes start report "already
    # running" and skip a real start.
    _pargs="\$(ps -p "\$_pf_pid" -o args= 2>/dev/null)"
    case "\$_pargs" in
        "\$daemon"*" -t $service_name "*"$provider_bin provide"*) echo "\$_pf_pid"; return 0 ;;
    esac
    return 1
}

${service_name}_start()
{
    if _pid="\$(\${name}_running_pid)"; then
        echo "$service_name is already running as pid \$_pid; not starting a second copy."
        return 0
    fi
    rm -f "\$pidfile"
    mkdir -p "\$logdir"
    chown root:wheel "\$logdir" 2>/dev/null || true
    chmod 0755 "\$logdir" 2>/dev/null || true
    # Pre-create stdout.log as a regular file owned by the service user.
    # Must be a regular file (not a symlink) to prevent privilege escalation
    # via planted symlinks. If it exists and is a symlink, refuse to start.
    if [ -L "\$logdir/stdout.log" ]; then
        echo "Refusing to start: \$logdir/stdout.log is a symlink (privilege escalation risk)."
        return 1
    fi
    # daemon(8) opens this path as root, so an existing log is kept only while
    # it is a plain file with a single link that the service user owns. A fifo,
    # a hard link or a foreign owner is replaced: removing the name leaves the
    # inode it pointed at alone.
    if [ -e "\$logdir/stdout.log" ] && [ "\$(stat -f '%HT %l %Su' "\$logdir/stdout.log" 2>/dev/null)" != "Regular File 1 \${${service_name}_user}" ]; then
        rm -f "\$logdir/stdout.log" || return 1
    fi
    # Create the log only when it is missing: installing over an existing
    # file truncates it, so every restart would erase the previous run.
    if [ ! -e "\$logdir/stdout.log" ]; then
        install -o \${${service_name}_user} -m 640 /dev/null "\$logdir/stdout.log" 2>/dev/null || return 1
    fi
    echo "Starting $service_name."
    \$daemon -f -t $service_name -p "\$pidfile" -o "\$logdir/stdout.log" \\
        -u \${${service_name}_user} \\
        "$provider_bin" provide \$${service_name}_flags
}

${service_name}_stop()
{
    echo "Stopping $service_name."
    if ! _pid="\$(\${name}_running_pid)"; then
        echo "$service_name is not running."
        rm -f "\$pidfile"
        return 0
    fi
    kill "\$_pid" 2>/dev/null || true
    _n=0
    while [ "\$_n" -lt 30 ]; do
        kill -0 "\$_pid" 2>/dev/null || break
        sleep 1
        _n=\$((_n + 1))
    done
    if kill -0 "\$_pid" 2>/dev/null; then
        echo "$service_name did not exit within 30s; sending SIGKILL."
        kill -9 "\$_pid" 2>/dev/null || true
    fi
    rm -f "\$pidfile"
}

${service_name}_status()
{
    if _pid="\$(\${name}_running_pid)"; then
        echo "$service_name is running as pid \$_pid."
        return 0
    fi
    echo "$service_name is not running."
    return 1
}

run_rc_command "\$1"
RCSCRIPT
}

# assert_state_path_safe runs BEFORE anything is installed. A symlink anywhere
# in the service user's own path sends root's mkdir and chmod somewhere else,
# and checking only the leaf misses a symlinked parent. It must happen early:
# refusing after the binary has been renamed into place leaves a half-done
# install. System ancestors above the home are not checked, because legitimately
# symlinked system paths (/home -> /usr/home) would make every install refuse.
assert_state_path_safe() {
    if [ -L "$service_home" ]; then
        pr_err "%s is a symlink; refusing to install through it" "$service_home"
        exit 1
    fi
    if [ -L "$state_dir" ]; then
        pr_err "%s is a symlink; refusing to create or chmod through it" "$state_dir"
        exit 1
    fi
    # In this layout the install tree is root-owned at /usr/local/libexec, so
    # its ancestors are system paths rather than the service user's. The tree
    # itself must still not be a symlink: root's mkdir, cp and mv would
    # otherwise be redirected somewhere else.
    if [ -L "$install_path" ]; then
        pr_err "%s is a symlink; refusing to install through it" "$install_path"
        exit 1
    fi
}

# enforce_install_tree_perms <tree> <binary>
#
# Repair the install tree's ownership and traversal, or return nonzero with the
# path and the failed repair in the message. Every command goes through priv,
# so the gate runs the same way for root and for a sudo user, and it is a GATE
# rather than best effort: rc.d starts the provider as root and daemon(8) drops
# to the service user before exec, so a tree that stays writable by that user
# crosses a privilege boundary and a tree it cannot traverse can never start.
# A repair that cannot be made returns nonzero and the caller aborts instead of
# writing the rc.d script and reporting success.
enforce_install_tree_perms() {
    _tree="$1"
    _bin="$2"
    _links=$(priv find "$_tree" -type l -print) || {
        pr_err "could not scan %s for symlinks" "$_tree"
        return 1
    }
    if [ -n "$_links" ]; then
        pr_err "symlink in install tree; refusing to repair: %s" "$_links"
        return 1
    fi
    _tree_conforms() {
        _dirs=$(priv find "$_tree" -type d -print) || return 1
        for _path in $_dirs; do
            [ "$(priv stat -f %u "$_path" 2>/dev/null)" = 0 ] || return 1
            [ "$(priv stat -f %Lp "$_path" 2>/dev/null)" = 755 ] || return 1
        done
        # Every regular file, not only $_bin: bin/urnet-tools is root-executed
        # too, and version is written by this run. Any group/other write bit
        # fails, and so does a mode that does not parse as three octal digits.
        _files=$(priv find "$_tree" -type f -print) || return 1
        for _path in $_files; do
            [ "$(priv stat -f %u "$_path" 2>/dev/null)" = 0 ] || return 1
            _mode=$(priv stat -f %Lp "$_path" 2>/dev/null) || return 1
            case "$_mode" in
                [0-7][2367][0-7]|[0-7][0-7][2367]) return 1 ;;
                [0-7][0-7][0-7]) ;;
                *) return 1 ;;
            esac
        done
        [ "$(priv stat -f %u "$_bin" 2>/dev/null)" = 0 ] || return 1
        [ "$(priv stat -f %Lp "$_bin" 2>/dev/null)" = 755 ]
    }
    # :wheel is the FreeBSD convention, not what makes the tree safe, so retry
    # with root alone before failing: the boundary is the OWNER, not the group.
    if ! priv chown -R root:wheel "$_tree" 2>/dev/null; then
        if ! priv chown -R root "$_tree" 2>/dev/null; then
            if _tree_conforms; then
                pr_info "chown -R root:wheel and chown -R root both failed; %s already conformed" "$_tree"
            else
                pr_err "could not make %s root-owned: chown -R root:wheel and chown -R root both failed" "$_tree"
                return 1
            fi
        fi
    fi
    # Group/other write on a binary root executes is the boundary itself, so
    # this is required rather than convenient.
    if ! priv chmod -R go-w "$_tree" 2>/dev/null; then
        if _tree_conforms; then
            pr_info "chmod -R go-w failed; %s already conformed" "$_tree"
        else
            pr_err "could not remove group/other write access from %s: chmod -R go-w failed" "$_tree"
            return 1
        fi
    fi
    # One path per call so the message names the directory that failed: 0755 on
    # bin/ is no use while the tree above it is still 0700.
    for _dir in "$_tree" "$_tree/bin"; do
        if ! priv chmod 755 "$_dir" 2>/dev/null; then
            if _tree_conforms; then
                pr_info "chmod 755 failed for %s; %s already conformed" "$_dir" "$_tree"
            else
                pr_err "could not make %s traversable by the service user: chmod 755 failed" "$_dir"
                return 1
            fi
        fi
    done
    # The staged binary must be executable before it replaces the live image.
    if ! priv chmod 755 "$_bin" 2>/dev/null; then
        if _tree_conforms; then
            pr_info "chmod 755 failed for %s; %s already conformed" "$_bin" "$_tree"
        else
            pr_err "could not make %s executable by the service user: chmod 755 failed" "$_bin"
            return 1
        fi
    fi
    # Successful repair commands alone do not prove ownership or modes.
    if ! _tree_conforms; then
        pr_err "could not verify ownership and permissions of %s" "$_tree"
        return 1
    fi
    return 0
}

do_install() {
    # Every exit from here on removes a staged file that priv created, so the
    # cleanup runs on all of them. The signal traps exit normally (130 = SIGINT,
    # 143 = SIGTERM, 129 = SIGHUP) so the EXIT trap runs and the status still
    # says the install was interrupted rather than reporting success.
    trap install_cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    # Before the download, before the binary, before any root write.
    assert_state_path_safe
    version="${1:-latest}"

    # Refuse BEFORE downloading: an install that runs the internet-facing
    # relay as root is a privilege escalation the operator never asked for.
    # resolve_service_user only settles on root when the caller is root with
    # no sudo parent and names no override, so this never blocks a normal
    # `sudo install` (SUDO_USER is used) - only a genuine root login.
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user (sudo is fine: SUDO_USER is used),"
        pr_err "or name an account: SERVICE_USER_OVERRIDE=<user> %s install" "$me"
        exit 1
    fi

    if [ "$version" = "latest" ]; then
        release_url="$github_api/releases/latest"
    else
        release_url="$github_api/releases/tags/$version"
    fi

    pr_info "Fetching release info..."
    release_json="$(http_fetch "$release_url" 2>/dev/null)"
    # python3 is not part of the FreeBSD base system, so parsing with it alone
    # makes a default `latest` install fail on a clean host. jq first, then
    # python3, then a sed extraction that needs nothing beyond base.
    tag=""
    if command -v jq > /dev/null 2>&1; then
        tag="$(printf '%s' "$release_json" | jq -r '.tag_name // empty' 2>/dev/null)"
    fi
    if [ -z "$tag" ] && command -v python3 > /dev/null 2>&1; then
        tag="$(printf '%s' "$release_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tag_name") or "")' 2>/dev/null)"
    fi
    if [ -z "$tag" ]; then
        tag="$(printf '%s' "$release_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    fi

    # GitHub API failed or rate-limited: for an explicit version we can trust
    # the caller's tag directly; for "latest" fall back to the dl.fullbars.xyz
    # Worker, which mirrors GitHub's latest-release tag at the edge.
    if [ -z "$tag" ]; then
        if [ "$version" != "latest" ]; then
            tag="$version"
        else
            pr_warn "Trying dl.fullbars.xyz fallback..."
            tag="$(http_fetch "https://dl.fullbars.xyz/latest-version" 2>/dev/null | tr -d '[:space:]')"
        fi
    fi

    if [ -z "$tag" ]; then
        pr_err "Could not fetch release info from %s" "$release_url"
        exit 1
    fi

    arch="$(uname -m)"
    case "$arch" in
        amd64|x86_64)  goarch="amd64" ;;
        arm64|aarch64) goarch="arm64" ;;
        *)             pr_err "Unsupported architecture: %s" "$arch"; exit 1 ;;
    esac

    tarball_url="https://dl.fullbars.xyz/releases/download/$tag/urnetwork-provider-$tag-freebsd-$goarch.tar.gz"
    mirror_url="https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag-freebsd-$goarch.tar.gz"
    pr_info "Downloading %s..." "$tarball_url"

    tmpdir="$(mktemp -d)"
    if ! http_fetch "$tarball_url" "$tmpdir/provider.tar.gz"; then
        pr_warn "Primary download failed, trying GitHub mirror..."
        if ! http_fetch "$mirror_url" "$tmpdir/provider.tar.gz"; then
            pr_err "Failed to download from both primary and mirror"
            rm -rf "$tmpdir"
            exit 1
        fi
    fi

    tar -xzf "$tmpdir/provider.tar.gz" -C "$tmpdir" || {
        pr_err "Extract failed"
        rm -rf "$tmpdir"
        exit 1
    }

    # The FreeBSD archives ship the binary at the archive root; older layouts
    # nested it under freebsd/<arch>/. Accept both rather than exiting on a
    # valid release.
    provider_src="$tmpdir/provider"
    if [ ! -f "$provider_src" ]; then
        provider_src="$tmpdir/freebsd/$goarch/provider"
    fi
    if [ ! -f "$provider_src" ]; then
        pr_err "FreeBSD/%s binary not found in release tarball" "$goarch"
        rm -rf "$tmpdir"
        exit 1
    fi

    # The tree is root-owned at /usr/local/libexec, which a normal user cannot
    # create. Every write below goes through priv so a non-root install
    # elevates instead of failing at the first mkdir. (This was a regression
    # from moving the tree out of the service user's home.)
    priv mkdir -p "$install_path/bin" || { pr_err "Failed to create %s" "$install_path"; rm -rf "$tmpdir"; exit 1; }
    # `cp` onto a running binary fails with ETXTBSY ("Text file busy"), which
    # is exactly the `update` case: the provider is still executing the file
    # being replaced. Staging next to the target and renaming is atomic, is
    # allowed over a running image, and never leaves a half-written binary.
    # Staging an empty or non-executable file produces a provider that fails at
    # exec time, after the old binary has already been renamed away.
    # -f and -s, not -x: the binary is chmod 755 after staging, so a tarball
    # that lost the exec bit installs fine, while a directory or an empty file
    # still has to be refused before it replaces a working binary.
    if [ ! -f "$provider_src" ] || [ ! -s "$provider_src" ]; then
        pr_err "Refusing to install %s: not a non-empty regular file" "$provider_src"
        rm -rf "$tmpdir"; exit 1
    fi
    staged="$install_path/bin/.urnetwork.new.$$"
    # Recorded before cp, not after: a cp that dies halfway (ENOSPC, EINTR,
    # sudo timeout) may already have created the file as root, and that is
    # exactly the partial file the cleanup has to remove.
    staged_bin="$staged"
    priv cp "$provider_src" "$staged" || { pr_err "Failed to stage binary"; priv_rm "$staged"; rm -rf "$tmpdir"; exit 1; }
    priv chmod 755 "$staged"
    # The release tool is installed only when its digest can be verified.
    tool_asset="urnet-tools-freebsd-$goarch"
    # jq prints "null" for a missing field, so accept only a real hex digest.
    tool_digest=""
    release_json="$(http_fetch "$github_api/releases/tags/$tag" 2>/dev/null)"
    if command -v jq > /dev/null 2>&1; then
        tool_digest="$(printf '%s' "$release_json" | jq -r --arg a "$tool_asset" '
            (.assets[]? | select(.name == $a) | .digest)
            | select(type == "string")
            | sub("^sha256:"; "")
            | select(test("^[0-9a-fA-F]{64}$"))
        ' 2>/dev/null || true)"
    elif command -v python3 > /dev/null 2>&1; then
        tool_digest="$(printf '%s' "$release_json" | python3 -c '
import sys, json, re
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
val = ""
for asset in doc.get("assets", []):
    if asset.get("name") == sys.argv[1]:
        val = asset.get("digest") or ""
        break
val = val.replace("sha256:", "")
print(val if re.fullmatch(r"[0-9a-fA-F]{64}", val) else "")
' "$tool_asset" 2>/dev/null || true)"
    fi

    if [ -z "$tool_digest" ]; then
        pr_err "Cannot verify urnet-tools: release digest is unavailable"
        exit 1
    fi
    if ! http_fetch "https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/$tool_asset" "$tmpdir/$tool_asset" 2>/dev/null; then
        pr_err "Failed to download urnet-tools"
        exit 1
    fi
    staged_tool="$install_path/bin/.urnet-tools.new.$$"
    priv cp "$tmpdir/$tool_asset" "$staged_tool" || { pr_err "Failed to stage urnet-tools"; exit 1; }
    priv chmod 755 "$staged_tool" || { pr_err "Failed to make urnet-tools executable"; exit 1; }
    # Verify the staged file itself, not only the downloaded source.
    if command -v sha256 > /dev/null 2>&1; then
        actual="$(sha256 -q "$staged_tool")"
    elif command -v openssl > /dev/null 2>&1; then
        actual="$(openssl dgst -sha256 "$staged_tool" | awk '{print $NF}')"
    else
        actual=""
    fi
    if [ -z "$actual" ] || [ "$actual" != "$tool_digest" ]; then
        pr_err "Cannot verify urnet-tools: digest mismatch or no checksum tool"
        exit 1
    fi

    # On gate failure, live binaries stay untouched, permission repairs remain,
    # and an error is reported. Check the tree before any live replacement.
    if ! enforce_install_tree_perms "$install_path" "$staged"; then
        rm -rf "$tmpdir"
        exit 1
    fi

    # The install tree stays ROOT-OWNED and group/other-writable-never. The
    # provider binary is executed by root on every hot-swap and by the rc.d
    # script on restart, so a service-user-writable binary is a privilege
    # boundary the installer must not create. Only the STATE directory belongs
    # to the service user: that is where its own config and JWT live.
    if [ "$(id -u)" = "0" ]; then
        if [ "$service_user" != "root" ]; then
            # Re-check here, not only at the top of do_install: the service user
            # controls this directory and can replace it with a symlink between
            # the two, and these are the root writes that would follow it.
            if [ -L "$state_dir" ]; then
                pr_err "%s became a symlink during install; refusing to write through it" "$state_dir"
                rm -rf "$tmpdir"; exit 1
            fi
            # No -p: it follows a symlinked state_dir, and the parent (the
            # service user's home) already exists.
            [ -d "$state_dir" ] || mkdir "$state_dir" 2>/dev/null || true
            # Enter the directory before re-owning it, for the same reason as
            # the check above: cd resolves the entry once, and the relative
            # '.' cannot be redirected by a symlink swapped in afterwards. The
            # cd and the writes share ONE subshell: a cd in its own subshell
            # leaves the installer's working directory unchanged, and the
            # recursive chown would then run on wherever it was started from.
            # The directory entered must also be the one the name should
            # reach, or a symlink swapped in after the check above lands root
            # in an arbitrary directory.
            (
                cd "$state_dir" 2>/dev/null || exit 1
                [ "$(pwd -P)" = "$(cd "$service_home" 2>/dev/null && pwd -P)/.urnetwork" ] || exit 2
                # Not recursive: the service user owns this tree, and a hard
                # link it plants to a root-owned file would be re-owned by a
                # recursive root chown (-h guards symlinks, not links).
                chown -h "$service_user" . 2>/dev/null || true
                chmod 700 . 2>/dev/null || true
                if [ -n "$(find . ! -user "$service_user" -print 2>/dev/null | head -n 1)" ]; then
                    pr_warn "%s holds files not owned by %s; fix them by hand" "$state_dir" "$service_user"
                fi
            ) || pr_warn "left %s alone: it could not be entered, or it resolves outside %s" "$state_dir" "$service_home"
        fi
    fi

    priv mv -f "$staged_tool" "$install_path/bin/urnet-tools" || { pr_err "Failed to install urnet-tools"; exit 1; }
    staged_tool=""
    priv mv -f "$staged" "$provider_bin" || { pr_err "Failed to install binary"; priv_rm "$staged"; rm -rf "$tmpdir"; exit 1; }
    # Renamed into place: forget the name, or the EXIT trap would delete the
    # binary that is now the installed provider.
    staged_bin=""

    # Same rename-not-follow rule as the binary above. A symlink planted at the
    # version name would redirect the write, so stage beside it and rename over
    # it: rename replaces the entry instead of following it.
    ver_tmp="$install_path/.version.new.$$"
    staged_version="$ver_tmp"
    # Not fatal: the binary is already installed and the provider is about to
    # start, so failing here would report a failed install for a working
    # provider. The root-owned temp file still has to go with the failure, or
    # every retry leaves .version.new.<pid> in the install tree.
    if ! { printf '%s\n' "$tag" | priv tee "$ver_tmp" > /dev/null &&
           priv mv -f "$ver_tmp" "$install_path/version"; }; then
        priv_rm "$ver_tmp"
        pr_warn "could not write %s; version keeps reporting the previous value" "$install_path/version"
    fi
    staged_version=""
    priv chmod 644 "$install_path/version" 2>/dev/null || true


    if [ "$(id -u)" != "0" ]; then
        pr_info "Installing the rc.d service requires root; re-running that step with sudo..."
        if command -v sudo > /dev/null 2>&1; then
            sudo mkdir -p "$(dirname "$rc_script")"
            sudo tee "$rc_script" > /dev/null <<RCSCRIPT
$(write_rc_script_body "$service_user")
RCSCRIPT
            sudo chmod 755 "$rc_script"
            # Only when unset: an operator's explicit enable/user survives an update.
            set_rc_conf_default "${service_name}_enable" YES
            set_rc_conf_default "${service_name}_user" "$service_user"
        else
            pr_warn "sudo not found. Install the service manually:"
            pr_warn "  sudo mkdir -p /usr/local/etc/rc.d"
            pr_warn "  sudo %s install-service" "$me"
        fi
    else
        mkdir -p "$(dirname "$rc_script")"
        write_rc_script_body "$service_user" > "$rc_script"
        chmod 755 "$rc_script"
        # Only when unset: an operator's explicit enable/user survives an update.
        set_rc_conf_default "${service_name}_enable" YES
        set_rc_conf_default "${service_name}_user" "$service_user"
    fi

    # Start policy: a fresh install starts the service; an update restarts it
    # only if it was already running, so a deliberately stopped provider is not
    # resurrected by an unattended `update` from cron. Failures are fatal —
    # reporting "installed" for a service that did not start is a false green.
    if service_running; then
        restart_service || { pr_err "Failed to restart the service after install"; exit 1; }
    elif [ "${UPDATING:-0}" != "1" ]; then
        load_service || { pr_err "Failed to start the service"; exit 1; }
    else
        pr_info "Service is not running; leaving it stopped (run '%s start' when ready)" "$me"
    fi

    # PATH hint goes into the SERVICE user's shell rc, once. Under sudo $HOME
    # is root's, and root's PATH never contains the service user's bin dir, so
    # the old check re-appended a duplicate line on every install and update.
    if [ "${UPDATING:-0}" != "1" ]; then
        _shell="$(user_shell "$service_user")"
        shell_rc="$service_home/.profile"
        case "$_shell" in
            */zsh)  shell_rc="$service_home/.zshrc" ;;
            */bash) shell_rc="$service_home/.bash_profile" ;;
            */tcsh|*/csh) shell_rc="$service_home/.cshrc" ;;
        esac
        # The service user owns this directory, so a symlink planted at the rc
        # file's name sent root's append (and chown) into a file of their
        # choosing. Refuse a link outright; the provider is installed by now,
        # so warn and carry on instead of failing the whole install.
        if [ -L "$shell_rc" ]; then
            pr_warn "Refusing to edit %s: it is a symlink. Add %s/bin to PATH by hand" "$shell_rc" "$install_path"
        else
            case "$_shell" in
                */tcsh|*/csh) rc_line="setenv PATH \"$install_path/bin:\$PATH\"" ;;
                *)      rc_line="export PATH=\"$install_path/bin:\$PATH\"" ;;
            esac
            # How to reach the service user is settled BEFORE the script is fed
            # in. `su` from a caller that is neither root nor that user stops to
            # ask for a password, and a prompt during an unattended install is a
            # hang, not a hint. Root switches user; the service user edits their
            # own rc directly; anyone else is told to add the line by hand.
            shellrc_mode=""
            if [ "$(id -u)" = "0" ]; then
                shellrc_mode=su
            elif [ "$(id -un)" = "$service_user" ]; then
                shellrc_mode=direct
            fi
            if [ -z "$shellrc_mode" ]; then
                pr_warn "Skipping the PATH update in %s: it belongs to %s, but this install is running as %s. Add %s/bin to PATH by hand" \
                    "$shell_rc" "$service_user" "$(id -un)" "$install_path"
            else
                # Everything below runs AS THE SERVICE USER. Root must not
                # reopen a path inside a directory that user owns: between the
                # check above and any root cat/chmod/chown/mv the name can be
                # swapped for a link, and root would follow it. The owner
                # editing their own rc crosses no privilege boundary, so the
                # whole edit — copy, append, mode, rename — is handed over and
                # the installer only reports on it.
                #
                # The script goes in on stdin and is run by /bin/sh explicitly.
                # `su -c` would otherwise hand it to the login shell, and a tcsh
                # service user cannot parse POSIX sh. The login name must come
                # BEFORE -c: FreeBSD reads an earlier -c as a login class, not
                # as the command (its -m keeps the environment, so these
                # assignments reach the script).
                (
                    # One script, one environment, two ways in: root has to
                    # become the owner, the owner just runs it.
                    if [ "$shellrc_mode" = su ]; then
                        SHELLRC_RC="$shell_rc" SHELLRC_LINE="$rc_line" SHELLRC_BIN="$install_path/bin" \
                            su -m "$service_user" -c '/bin/sh -s'
                    else
                        SHELLRC_RC="$shell_rc" SHELLRC_LINE="$rc_line" SHELLRC_BIN="$install_path/bin" \
                            /bin/sh -s
                    fi
                ) <<'SHELLRC_END'
# Runs as the service user, who owns both the rc file and its directory.
# Exit: 0 wrote it, 10 already active, anything else failed and left the rc
# byte-for-byte alone (the temp is removed on every failure path).
set -u
rc="${SHELLRC_RC:-}"
line="${SHELLRC_LINE:-}"
bin="${SHELLRC_BIN:-}"
[ -n "$rc" ] && [ -n "$line" ] && [ -n "$bin" ] || exit 7
# mv would move *into* a directory and cat would block forever on a FIFO.
if [ -e "$rc" ] || [ -L "$rc" ]; then
    [ -f "$rc" ] || exit 8
fi
# Only an ACTIVE entry counts: a line whose first non-space character is not
# '#'. A commented-out export used to suppress the append for good.
if [ -f "$rc" ]; then
    if awk -v want="$bin" '
        { line = $0; sub(/^[[:space:]]+/, "", line) }
        line !~ /^#/ && index(line, want) > 0 { found = 1; exit }
        END { exit(found ? 0 : 1) }
    ' "$rc"; then
        exit 10
    fi
fi
# mktemp is O_EXCL and runs as the owner: no name can be planted ahead of it.
tmp="$(mktemp "$rc.new.XXXXXX")" || exit 3
if [ -f "$rc" ] && ! cat "$rc" > "$tmp"; then
    rm -f "$tmp"
    exit 4
fi
# Without this the entry is glued onto the last line; when the file already
# ends with a newline, adding another would leave a blank line.
last="$(tail -c 1 "$tmp" 2>/dev/null | wc -l | tr -d ' ')"
if [ -s "$tmp" ] && [ "${last:-0}" -eq 0 ]; then
    ( printf '\n' ) >> "$tmp" || { rm -f "$tmp"; exit 5; }
fi
# Subshell so a failed write cannot take this script down before the cleanup;
# the status is checked, and a half-written temp is never renamed over the rc.
( printf '%s\n' "$line" ) >> "$tmp" || { rm -f "$tmp"; exit 5; }
mode="$(stat -c '%a' "$rc" 2>/dev/null || stat -f '%Lp' "$rc" 2>/dev/null)"
chmod "${mode:-644}" "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 6; }
mv -f "$tmp" "$rc" || { rm -f "$tmp"; exit 6; }
SHELLRC_END
                case "$?" in
                    0)  pr_info "Added %s to PATH in %s" "$install_path/bin" "$shell_rc" ;;
                    10) : ;;
                    *)  pr_warn "Could not update %s; add %s/bin to PATH by hand" "$shell_rc" "$install_path" ;;
                esac
            fi
        fi
    fi

    rm -rf "$tmpdir"

    pr_info "URnetwork provider %s installed" "$tag"
    pr_info "  Binary:  %s" "$provider_bin"
    pr_info "  Service: %s" "$rc_script"
    pr_info "  Data:    %s" "$state_dir"
    pr_info ""
    pr_info "Commands:  urnet-tools start|stop|restart|status|hot-restart|session|proxy"
    pr_info "Restart your terminal or run 'hash -r' for urnet-tools to be found"
}

do_install_service() {
    if [ "$(id -u)" != "0" ]; then
        pr_err "install-service needs root: sudo %s install-service" "$me"
        exit 1
    fi
    # Running the provider as root because a bootstrap was run with sudo, or
    # because the box was logged into as root, would be a privilege
    # escalation the operator never asked for: refuse and say what to do.
    service_user="$(resolve_service_user)"
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user, or set SERVICE_USER_OVERRIDE=<user>"
        exit 1
    fi
    mkdir -p "$(dirname "$rc_script")"
    write_rc_script "$service_user"
    sysrc "$service_name"_enable=YES
    sysrc "$service_name"_user="$service_user"
    pr_info "Service installed at %s (runs as %s)" "$rc_script" "$service_user"
}

# --- runtime commands ---

do_start() {
    if service_running; then
        pr_info "Provider is already running"
        return 0
    fi
    if load_service; then
        pr_info "Provider started"
    else
        pr_err "Failed to start the provider"
        exit 1
    fi
}

do_stop() {
    if unload_service; then
        pr_info "Provider stopped"
    else
        pr_err "Failed to stop the provider"
        exit 1
    fi
}

do_restart() {
    if [ "$FORCE" != "1" ]; then
        printf "Restart the provider? [y/N]: "
        read -r yn < /dev/tty
        case "$yn" in
            [Yy]*) ;;
            *) pr_info "Aborted."; exit 0 ;;
        esac
    fi
    if restart_service; then
        pr_info "Provider restarted"
    else
        pr_err "Failed to restart the provider"
        exit 1
    fi
}

do_status() {
    if service_running; then
        pr_info "Provider is running"
    else
        pr_info "Provider is stopped"
    fi
}

do_version() {
    if [ -f "$install_path/version" ]; then
        cat "$install_path/version"
    else
        pr_err "Provider not installed"
        exit 1
    fi
}

do_update() {
    version="${1:-latest}"
    # Tells do_install to restart (not start) the service and to leave the
    # operator's rc.conf choices and shell rc alone.
    UPDATING=1
    do_install "$version"
    pr_info "Update complete."
}

# write_state_flag sets a flag file (on/off) under state_dir.
#
# state_dir lives in the service user's home, so under sudo the writer is root
# while every path component below that home belongs to the service user. No
# check-then-write on those paths can be atomic, so the write enters the
# directory once and then uses only relative paths:
#
#   - After `cd`, the shell's working directory is pinned to the inode it
#     resolved. Swapping state_dir for a symlink afterwards cannot move the
#     write, which closes the window between a `[ -L ]` check and the write.
#   - Root refuses to create the directory and refuses one it does not own:
#     `mkdir -p` follows a symlinked state_dir, and a directory that is not
#     the service user's is not one root should be writing into.
#   - The temp file is created with O_EXCL (`set -C`, noclobber) so a name
#     planted at it makes the write fail instead of redirecting it, and it is
#     renamed within the same directory: rename(2) replaces a planted dirent
#     rather than following it and never degrades to a cross-filesystem copy.
#   - umask 022 gives the 0644 the old `printf >` produced, without a separate
#     chmod (chmod follows symlinks).
write_state_flag() {
    _name="$1"
    _val="$2"
    _owner=""
    if [ "$(id -u)" = "0" ] && [ "$service_user" != "root" ]; then
        _owner="$service_user"
    fi

    if [ -L "$state_dir" ]; then
        pr_err "refusing to write under %s: it is a symlink" "$state_dir"
        return 1
    fi

    if [ -n "$_owner" ]; then
        # Root must not create the directory: mkdir follows a symlinked
        # state_dir, and the directory belongs to the service user anyway.
        if [ ! -d "$state_dir" ]; then
            pr_err "%s does not exist; start the provider once as %s, then retry" "$state_dir" "$_owner"
            return 1
        fi
    elif ! mkdir -p "$state_dir" 2>/dev/null; then
        pr_err "cannot create %s" "$state_dir"
        return 1
    fi

    # Subshell: the cd must not move the caller's working directory.
    (
        cd "$state_dir" 2>/dev/null || exit 1
        # The directory actually entered must be the service user's: a symlink
        # already in place when cd ran would otherwise land root elsewhere.
        if [ -n "$_owner" ] && [ "$(stat -f '%Su' . 2>/dev/null)" != "$_owner" ]; then
            pr_err "refusing to write under %s: it is not owned by %s" "$state_dir" "$_owner"
            exit 2
        fi
        # A directory planted at the flag name would make mv move the temp
        # file into it instead of replacing the entry.
        if [ -d "./$_name" ]; then
            exit 1
        fi
        _i=0
        while [ "$_i" -lt 10 ]; do
            _tmp="./.urnet-flag.$$.$_i"
            if ( set -C; umask 022; printf '%s\n' "$_val" > "$_tmp" ) 2>/dev/null; then
                mv -f "$_tmp" "./$_name" 2>/dev/null && exit 0
                rm -f "$_tmp" 2>/dev/null
                exit 1
            fi
            _i=$((_i + 1))
        done
        exit 1
    ) || {
        _status=$?
        # 2 is the ownership refusal, which already printed its own reason.
        [ "$_status" = "2" ] || pr_err "cannot write %s" "$state_dir/$_name"
        return 1
    }
}

do_hot_restart() {
    mode="${1:-}"
    file="$state_dir/hot_restart"
    case "$mode" in
        on)    write_state_flag hot_restart on  || exit 1; pr_info "hot-restart enabled" ;;
        off)   write_state_flag hot_restart off || exit 1; pr_info "hot-restart disabled" ;;
        status|"")
            if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                pr_info "hot-restart: on"
            else
                pr_info "hot-restart: off"
            fi
            ;;
        *) pr_err "Usage: urnet-tools hot-restart <on|off|status>"; exit 1 ;;
    esac
}

do_session() {
    action="${1:-}"
    shift 2>/dev/null || true
    case "$action" in
        save)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session save <file>"; exit 1; }
            mkdir -p "$state_dir"
            "$provider_bin" session-save "$1" || { pr_err "session save failed"; exit 1; }
            pr_info "Session saved to %s" "$1"
            ;;
        load)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session load <file>"; exit 1; }
            "$provider_bin" session-load "$1" || { pr_err "session load failed"; exit 1; }
            pr_info "Session loaded; restarting"
            restart_service
            ;;
        *) pr_err "Usage: urnet-tools session <save|load> <file>"; exit 1 ;;
    esac
}

do_proxy() {
    action="${1:-}"
    case "$action" in
        refresh)     "$provider_bin" proxy-refresh ;;
        remove-dead) "$provider_bin" proxy-remove-dead ;;
        summary)     "$provider_bin" proxy-summary ;;
        *)           pr_err "Usage: urnet-tools proxy <refresh|remove-dead|summary>"; exit 1 ;;
    esac
}

# --- arg parsing ---

FORCE=0
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)    show_help; exit 0 ;;
        -v|--version) do_version; exit 0 ;;
        -f|--force)   FORCE=1; shift ;;
        *)            break ;;
    esac
done

operation="${1:-}"
[ -n "$operation" ] && shift

case "$operation" in
    install)         do_install "$@" ;;
    install-service) do_install_service ;;
    update)          do_update "$@" ;;
    start)           do_start ;;
    stop)            do_stop ;;
    restart)         do_restart ;;
    status)          do_status ;;
    version)         do_version ;;
    hot-restart)     do_hot_restart "$@" ;;
    self-heal)
        file="$state_dir/proxy_self_heal"
        case "${1:-}" in
            on) write_state_flag proxy_self_heal on  || exit 1; pr_info "Self-heal enabled" ;;
            off) write_state_flag proxy_self_heal off || exit 1; pr_info "Self-heal disabled" ;;
            status|"")
                if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                    pr_info "self-heal: on"
                else
                    pr_info "self-heal: off"
                fi
                if [ -f "$state_dir/pressure_status" ]; then
                    if command -v jq >/dev/null 2>&1; then
                        pr_info "$(jq -r '"pressure: \(.score) (target_pool=\(.target_pool), updated=\(.updated))"' \
                            "$state_dir/pressure_status" 2>/dev/null)"
                    else
                        cat "$state_dir/pressure_status"
                    fi
                fi
                ;;
            *) pr_err "Usage: urnet-tools self-heal [on|off|status]"; exit 1 ;;
        esac
        ;;
    session)         do_session "$@" ;;
    proxy)           do_proxy "$@" ;;
    auth)            "$provider_bin" auth "$@" ;;
    logs)            "$provider_bin" logs "$@" ;;
    "")
        show_help
        exit 0
        ;;
    *)
        pr_err "Unknown command: %s" "$operation"
        echo "Run '$me --help' for usage."
        exit 1
        ;;
esac