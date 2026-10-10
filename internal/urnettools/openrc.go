//go:build linux

package urnettools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file is the OpenRC lifecycle backend: on hosts where OpenRC is the
// running init system (Alpine and friends) and systemd is not available,
// lifecycle commands route through rc-service / rc-update, and the
// auto-update schedule is a busybox crond periodic entry instead of the
// systemd timer. The systemd path (lifecycle_unix.go) is untouched: the
// backend is selected by RUNTIME DETECTION, never by GOOS — OpenRC is still
// linux, so only the running init system distinguishes the two.
//
// Explicit limit (matches issue #786): there is no zero-downtime HotSwap
// under OpenRC. supervise-daemon has no sd_notify MainPID handoff, so an
// in-process baton handoff would orphan the successor while supervise-daemon
// respawns a second provider. Updates therefore decline HotSwap cleanly
// (openrcHotSwapDecline) and restart the service stop/start.

const (
	// openrcServiceName is the OpenRC service name. It matches the init
	// script name the installer writes (/etc/init.d/urnetwork), so
	// `rc-service urnetwork ...` addresses exactly the supervised provider.
	openrcServiceName = "urnetwork"

	// openrcUpdateCronName is the periodic entry that runs the auto-update.
	openrcUpdateCronName = "urnetwork-update"
	// openrcUpdateCronPausedName is the entry's name while the installer has
	// it paused around a binary swap (see the shell installer); cleanup and
	// `auto-update off` must sweep it too.
	openrcUpdateCronPausedName = openrcUpdateCronName + ".installer-paused"
)

var (
	// openrcInitScriptPath marks the service as installed. A var so tests
	// can point it at a temp file.
	openrcInitScriptPath = "/etc/init.d/" + openrcServiceName
	// openrcPeriodicBaseDir is busybox crond's periodic queue root. The
	// auto-update entry is dropped into its daily/weekly/monthly subdirs;
	// Alpine's stock /etc/crontabs/root runs those dirs via run-parts.
	// A var so tests never touch the real /etc.
	openrcPeriodicBaseDir = "/etc/periodic"

	// Detection and execution seams (tests override).
	openrcProbeFn      = probeOpenRC
	openrcLookPathFn   = exec.LookPath
	openrcStatFn       = os.Stat
	systemdRunningFn   = systemdRunning
	openrcRunFn        = runOpenRCCommand
	openrcToolPathFn   = openrcToolPath
	openrcExecutableFn = os.Executable

	// providerSupervisedByOpenRCFn reports whether a provider process is the
	// direct child of supervise-daemon. Seam for tests.
	providerSupervisedByOpenRCFn = providerSupervisedByOpenRC
)

// systemdRunning reports whether systemd is the running init system. The
// standard marker is /run/systemd/system, which systemd creates at boot; the
// mere presence of a systemctl binary is not enough (containers and
// non-systemd hosts ship stubs).
func systemdRunning() bool {
	_, err := openrcStatFn("/run/systemd/system")
	return err == nil
}

// openrcPathExists is the stat probe used by the detection helpers.
func openrcPathExists(path string) bool {
	_, err := openrcStatFn(path)
	return err == nil
}

// probeOpenRC reports whether this host has an OPERATIONAL OpenRC. rc-service
// must be resolvable (it is the command every lifecycle operation runs) AND
// /run/openrc must exist: that directory is created by OpenRC at boot, and
// without it rc-service refuses every action ("You are attempting to run an
// openrc service on a system which openrc did not boot"), so a host that
// merely has the openrc package installed must NOT have its lifecycle routed
// here — the unitless paths keep working there.
func probeOpenRC() bool {
	haveRcService := false
	if _, err := openrcLookPathFn("rc-service"); err == nil {
		haveRcService = true
	}
	if !haveRcService {
		for _, p := range []string{"/sbin/rc-service", "/usr/sbin/rc-service"} {
			if openrcPathExists(p) {
				haveRcService = true
				break
			}
		}
	}
	if !haveRcService {
		return false
	}
	return openrcPathExists("/run/openrc")
}

// openrcActive reports whether lifecycle commands should route through the
// OpenRC backend. Systemd wins whenever it is the RUNNING init: a host where
// systemd is up keeps every existing systemd behavior byte-for-byte, even if
// an OpenRC package happens to be installed alongside.
func openrcActive() bool {
	if !openrcProbeFn() {
		return false
	}
	return !systemdRunningFn()
}

// openrcServiceInstalled reports whether the OpenRC service exists (its init
// script is on disk). Without it there is nothing to start/stop/autostart and
// callers fall back to the unitless/bare-process paths.
func openrcServiceInstalled() bool {
	return openrcPathExists(openrcInitScriptPath)
}

// openrcToolPath resolves an OpenRC tool (rc-service, rc-update) to an
// absolute path: PATH first, then the standard sbin locations. A provider
// user's login PATH routinely lacks /sbin, but the tool must still find
// rc-service to manage the system service.
func openrcToolPath(name string) string {
	if p, err := openrcLookPathFn(name); err == nil {
		return p
	}
	for _, dir := range []string{"/sbin", "/usr/sbin"} {
		p := filepath.Join(dir, name)
		if openrcPathExists(p) {
			return p
		}
	}
	return name
}

// runOpenRCCommand runs an OpenRC tool with stdio passthrough so the operator
// sees its output (including rc-service's "already started" / "already
// stopped" notices).
func runOpenRCCommand(args ...string) error {
	if len(args) == 0 {
		return errors.New("empty OpenRC command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// openrcServiceArgv is the rc-service argv for an action on the service.
func openrcServiceArgv(action string) []string {
	return []string{openrcToolPathFn("rc-service"), openrcServiceName, action}
}

// openrcUpdateArgv is the rc-update argv for adding/removing the service.
// The runlevel is ALWAYS named explicitly: `rc-update del <service>` without
// one targets sysinit, not default, and reports "not in the runlevel".
func openrcUpdateArgv(action string) []string {
	return []string{openrcToolPathFn("rc-update"), action, openrcServiceName, "default"}
}

// openrcPeriodicIntervals are the busybox crond queues the auto-update entry
// can live in, mapped from the interval names the CLI already accepts.
var openrcPeriodicIntervals = []string{"daily", "weekly", "monthly"}

// openrcValidInterval reports whether interval names a periodic queue.
func openrcValidInterval(interval string) bool {
	for _, d := range openrcPeriodicIntervals {
		if d == interval {
			return true
		}
	}
	return false
}

// openrcSelectorMatches reports whether a discovered provider satisfies the
// target's selector flags. With no selectors set, every provider matches.
func openrcSelectorMatches(t Target, p Provider) bool {
	if t.User != "" && t.User != p.User {
		return false
	}
	if t.StateDir != "" && t.StateDir != p.StateDir {
		return false
	}
	if t.Network != "" && t.Network != p.Network {
		return false
	}
	if t.NetworkID != "" && t.NetworkID != p.NetworkID {
		return false
	}
	if t.PID > 0 && t.PID != p.PID {
		return false
	}
	return true
}

// openrcMatchedProvider returns the uniquely matched discovered provider for
// the target's selectors, when it is the process the OpenRC service
// supervises. A --pid (or any selector) match is never "ambiguous" just
// because OTHER providers exist: the selector names one process, and routing
// applies only when that process is supervise-daemon's child. With no
// selectors, exactly one discovered provider is required.
func openrcMatchedProvider(t Target) (Provider, bool) {
	return openrcMatchedProviderFrom(discoverSystemdFn(), t)
}

// openrcMatchedProviderFrom is openrcMatchedProvider against an ALREADY-TAKEN
// discovery snapshot.
//
// Why the snapshot is passed in (review finding R1): one command used to run
// Discover() up to four separate times — once in each selector check, and again
// for the confirm gate. Every call re-scans /proc, re-reads each provider's
// control socket and re-shells out to systemctl, so two checks inside the SAME
// command could disagree if the service restarted in between, and the PID
// liveness re-check that follows could read a recycled PID. Resolving once and
// passing the slice makes every decision in one command agree with every
// other by construction.
func openrcMatchedProviderFrom(provs []Provider, t Target) (Provider, bool) {
	var matches []Provider
	for _, p := range provs {
		if openrcSelectorMatches(t, p) {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return Provider{}, false
	}
	if !providerSupervisedByOpenRCFn(matches[0]) {
		return Provider{}, false
	}
	return matches[0], true
}

// openrcTargetApplies reports whether a parsed target names the OpenRC
// service (or names nothing at all). Any other explicit unit is a different
// provider and must not be hijacked by the OpenRC backend. Selector flags
// (--user/--state-dir/--network/--network-id/--pid) are only accepted when
// they match a discovered provider that IS the service's own supervised
// process — `stop --pid <bare-pid>` must not silently stop the service
// instead, and a bare provider must not satisfy a selector on the service's
// behalf. No unique supervised match falls through so the normal path's
// precise error runs rather than a guess.
func openrcTargetApplies(t Target) bool {
	return openrcTargetAppliesFrom(discoverSystemdFn(), t)
}

func openrcTargetAppliesFrom(provs []Provider, t Target) bool {
	if t.Unit != "" && t.Unit != openrcServiceName && t.Unit != openrcServiceName+".service" {
		return false
	}
	if t.User == "" && t.StateDir == "" && t.Network == "" && t.NetworkID == "" && t.PID == 0 {
		return true
	}
	_, ok := openrcMatchedProviderFrom(provs, t)
	return ok
}

// openrcLifecycleTargetApplies is openrcTargetApplies plus the running-target
// rule for start/stop/restart: with NO selectors and discovery showing running
// providers, the route applies only when at least one of them is the service's
// supervised process. If the service is stopped and only a BARE provider runs,
// the normal unitless path must handle it (matching the systemd path's
// preference for the running target over a stopped unit) — `rc-service stop`
// would otherwise falsely report success while the bare process keeps running.
// Zero discovered providers (a simply-stopped service) still routes, so
// `urnet-tools start` keeps working on a service-only host. The auto-start and
// auto-update routes deliberately do NOT use this: their periodic entry is not
// service-bound and stays useful for a bare provider.
func openrcLifecycleTargetApplies(t Target) bool {
	return openrcLifecycleTargetAppliesFrom(discoverSystemdFn(), t)
}

func openrcLifecycleTargetAppliesFrom(provs []Provider, t Target) bool {
	if !openrcTargetAppliesFrom(provs, t) {
		return false
	}
	if t.User != "" || t.StateDir != "" || t.Network != "" || t.NetworkID != "" || t.PID > 0 {
		return true // the selector already matched the supervised provider
	}
	// No selectors. Decide by what is actually RUNNING, which is what the user
	// means by "the provider":
	//   - nothing running: the service is the only candidate, so `start` works
	//     (this is the case a stopped service must not lose - and the reason
	//     this cannot simply test len(provs)==0: stopped-service discovery now
	//     reports the stopped service as a row, so the slice is NOT empty);
	//   - the service's own process is running: it is the target;
	//   - only some OTHER process runs: that is the user's provider, and the
	//     unitless path must handle it rather than `rc-service` reporting
	//     success while the bare process keeps running.
	// Several running at once still routes, so the ambiguity refusal below can
	// name them instead of silently picking one.
	running := 0
	serviceRunning := false
	for _, p := range provs {
		// A discovered row is a live process when it carries a pid; the
		// stopped-service record carries neither a pid nor Running.
		if p.PID <= 0 && !p.Running {
			continue
		}
		running++
		if providerSupervisedByOpenRCFn(p) {
			serviceRunning = true
		}
	}
	if running == 0 {
		return true
	}
	return serviceRunning
}

// openrcServiceLogPaths returns the stdout/stderr files the generated init
// script points supervise-daemon at, read back from the script itself rather
// than hardcoded, so a user who edited output_log keeps working.
func openrcServiceLogPaths() (stdout, stderr string) {
	stdout, stderr = "/var/log/urnetwork.log", "/var/log/urnetwork.err"
	f, err := os.Open(openrcInitScriptPath)
	if err != nil {
		return stdout, stderr
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "output_log=") {
			if v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "output_log=")), `"'`); v != "" {
				stdout = v
			}
		}
		if strings.HasPrefix(line, "error_log=") {
			if v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "error_log=")), `"'`); v != "" {
				stderr = v
			}
		}
	}
	return stdout, stderr
}

// openrcTailServiceLogs streams the OpenRC service's log files. handled is true
// only on an OpenRC host with an installed service, so every other platform
// falls through unchanged.
//
// Prefers stdout; falls back to stderr when only that one exists (a provider
// whose stdout is quiet). Reports the precise problem when neither is readable,
// rather than the generic "no logs".
func openrcTailServiceLogs(p Provider, lines int) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	stdout, stderr := openrcServiceLogPaths()
	target := ""
	for _, cand := range []string{stdout, stderr} {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			target = cand
			break
		}
	}
	if target == "" {
		return true, fmt.Errorf("OpenRC service %s has no readable log file (looked for %s and %s): the service may never have started, or supervise-daemon has no output redirection configured",
			openrcServiceName, stdout, stderr)
	}
	fmt.Printf("Streaming %s (%d lines) from the OpenRC service — provider %s\n", target, lines, providerLabel(p))
	cmd := exec.Command("tail", "-n", strconv.Itoa(lines), "-f", target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return true, cmd.Run()
}

// openrcOthersBeyondService names the discovered providers that are NOT the
// OpenRC service's own supervised process — i.e. the ones a selector-less
// stop/restart would silently leave running.
//
// This is only ever called for a SELECTOR-LESS target (the caller gates on it),
// and openrcSelectorMatches matches EVERY provider for such a target — so the
// exclusion has to be "is this the service's own supervised process", not "does
// it match the selector".
func openrcOthersBeyondService(provs []Provider, t Target) []string {
	var out []string
	for _, p := range provs {
		// The service's own record is not "another provider", whether it is
		// running (supervised) or stopped (discovered from the init script with
		// Supervisor "openrc"). Missing the stopped case made a bare
		// `urnet-tools stop` on a stopped service refuse with "2 providers
		// found", naming the service itself as the other one.
		if providerSupervisedByOpenRCFn(p) || p.Supervisor == "openrc" {
			continue
		}
		label := p.User
		if label == "" {
			label = "unknown"
		}
		if p.Network != "" {
			label += "@" + p.Network
		}
		if p.PID > 0 {
			label += fmt.Sprintf(" (pid %d)", p.PID)
		} else if p.Unit != "" {
			label += " (stopped unit " + p.Unit + ")"
		}
		out = append(out, label)
	}
	return out
}

// openrcGateProvider is the provider record used for the restart confirm gate
// and the restart-reason marker on an OpenRC host: the service name, enriched
// best-effort with the service's own supervised provider so the audit line
// shows the real user/state when they are unambiguous. The target's selectors
// are honored, so `restart --pid <service-pid>` enriches from exactly that
// process.
func openrcGateProvider(t Target) Provider {
	return openrcGateProviderFrom(discoverSystemdFn(), t)
}

func openrcGateProviderFrom(provs []Provider, t Target) Provider {
	p := Provider{Unit: openrcServiceName}
	if prov, ok := openrcMatchedProviderFrom(provs, t); ok {
		p.User = prov.User
		p.StateDir = prov.StateDir
		p.StateHome = prov.StateHome
		p.Network = prov.Network
		p.PID = prov.PID
		// Binary feeds openrcUpdateToolPath's first preference (the
		// urnet-tools beside the provider binary); without it the cron entry
		// would always fall back to this running executable.
		p.Binary = prov.Binary
	}
	return p
}

// openrcRouteLifecycle intercepts start/stop/restart on OpenRC hosts. handled
// is false when the OpenRC backend does not apply and the caller must run its
// normal systemd/unitless path (a different explicit unit, leftover
// positionals — which the normal path refuses with its precise message — or
// no OpenRC service installed).
func openrcRouteLifecycle(verb string, args []string, force, dryRun bool) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return false, nil // the normal path reports the parse error identically
	}
	if len(rest) > 0 {
		return false, nil
	}
	// ONE discovery snapshot for the whole command (review finding R1).
	provs := discoverSystemdFn()
	if !openrcLifecycleTargetAppliesFrom(provs, t) {
		return false, nil
	}
	// Ambiguity refusal for destructive verbs (review finding R2). With the
	// service plus some other provider on the box, `stop`/`restart` with no
	// selector would act on the OpenRC service ONLY and silently leave the
	// other one running — the same shape the systemd path refuses outright
	// ("2 providers found, specify a target"), and a false "stopped" report.
	// `start` is deliberately NOT gated: starting is not destructive, and a
	// selector-less start on a multi-provider box is the common first-run case
	// the routing exists to support.
	hasSelector := t.User != "" || t.StateDir != "" || t.Network != "" || t.NetworkID != "" || t.PID > 0
	if others := openrcOthersBeyondService(provs, t); !hasSelector && (verb == "stop" || verb == "restart") && len(others) > 0 {
		// Count the other providers PLUS the service, which is also a target.
		return true, fmt.Errorf(
			"%d providers found on this box, specify a target: %s\n"+
				"  urnet-tools %s --user <user>          # a specific provider\n"+
				"  urnet-tools %s --unit %s       # the OpenRC service\n"+
				"  urnet-tools providers             # list what was found",
			len(others)+1, others, verb, verb, openrcServiceName)
	}
	if dryRun {
		fmt.Printf("[dry-run] would %s %s (OpenRC service)\n", verb, openrcServiceName)
		return true, nil
	}
	if verb == "restart" {
		gateProvider := openrcGateProviderFrom(provs, t)
		ok, err := confirmGate("restart "+openrcServiceName+" (OpenRC service)", gateProvider, force, dryRun)
		if err != nil {
			return true, err
		}
		if !ok {
			return true, nil
		}
		// Leave the reason for the provider's next start (best effort).
		recordRestartReason(gateProvider, restartReasonManual)
	}
	fmt.Printf("running rc-service %s %s...\n", openrcServiceName, verb)
	if err := openrcRunFn(openrcServiceArgv(verb)...); err != nil {
		return true, fmt.Errorf("rc-service %s %s: %w%s", openrcServiceName, verb, err, openrcElevationHint())
	}
	fmt.Printf("%s %s (OpenRC service)\n", openrcVerbPast(verb), openrcServiceName)
	return true, nil
}

// openrcVerbPast returns the past-tense form for the lifecycle messages.
func openrcVerbPast(verb string) string {
	switch verb {
	case "start":
		return "started"
	case "stop":
		return "stopped"
	case "restart":
		return "restarted"
	}
	return verb
}

// openrcRouteAutoStart intercepts `auto-start on|off` on OpenRC hosts:
// rc-update add|del <service> default. Leftover positionals are refused (the
// normal path silently swallows them; acting on a mistyped command is worse).
func openrcRouteAutoStart(mode string, rest []string, force, dryRun bool) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	t, remaining, err := parseTargetFlags(rest)
	if err != nil {
		return false, nil
	}
	if len(remaining) > 0 {
		return true, fmt.Errorf("auto-start takes no positional arguments — got %q", strings.Join(remaining, " "))
	}
	if !openrcTargetApplies(t) {
		return false, nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would set auto-start %s for %s (OpenRC service)\n", mode, openrcServiceName)
		return true, nil
	}
	action := "add"
	if mode == "off" {
		action = "del"
	}
	if err := openrcRunFn(openrcUpdateArgv(action)...); err != nil {
		return true, fmt.Errorf("rc-update %s %s: %w%s", action, openrcServiceName, err, openrcElevationHint())
	}
	return true, nil
}

// openrcRouteAutoUpdate intercepts `auto-update daily|weekly|monthly|off` on
// OpenRC hosts: a busybox crond periodic entry replaces the systemd timer.
// Unlike auto-start it does not require the init script — the entry is a
// plain cron drop-in — but it runs the SAME urnet-tools binary the operator
// uses, resolved from the provider's install dir when possible.
func openrcRouteAutoUpdate(interval string, rest []string, dryRun bool) (bool, error) {
	if !openrcActive() {
		return false, nil
	}
	t, remaining, err := parseTargetFlags(rest)
	if err != nil {
		return false, nil
	}
	if len(remaining) > 0 {
		return true, fmt.Errorf("auto-update takes no positional arguments — got %q", strings.Join(remaining, " "))
	}
	if !openrcTargetApplies(t) {
		return false, nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would set auto-update %s for %s (busybox crond periodic entry)\n", interval, openrcServiceName)
		return true, nil
	}
	if err := openrcSetAutoUpdate(interval, openrcUpdateToolPath(openrcGateProvider(t))); err != nil {
		return true, err
	}
	if interval != "off" {
		if note := openrcCrondNote(); note != "" {
			fmt.Fprintf(os.Stderr, "note: %s\n", note)
		}
	}
	return true, nil
}

// openrcUpdateToolPath resolves the urnet-tools binary the auto-update cron
// entry must run. Preference order: the tool beside the provider binary (the
// canonical install layout the shell installer creates), then PATH (the
// installer also links /usr/local/bin/urnet-tools), then this running binary,
// then a bare name. PATH deliberately precedes this executable: a tool run
// from a transient copy (a fresh build in /tmp) must not bake that path into
// a persistent cron entry.
// openrcRootToolPath is where the installer stages a ROOT-OWNED copy of
// urnet-tools. It must match openrc_root_tool_path in
// scripts/Provider_Install_Linux.sh.
const openrcRootToolPath = "/usr/local/libexec/urnetwork/urnet-tools"

// openrcInstallRoot is the ROOT-OWNED OpenRC install tree. It must match
// openrc_install_root in scripts/Provider_Install_Linux.sh. The tree lives
// here — NOT under the service user's home — so root-executed code (the
// weekly cron entry, sudo urnet-tools update) never reaches a binary through
// a user-writable ancestor. A tree under /home/<user> is renameable aside by
// its owner, and root-owned files cannot defend a path whose ancestor is
// user-writable.
const openrcInstallRoot = "/usr/local/lib/urnetwork-provider"

// openrcTreeOwnedByRoot reports whether a binary path lives under the
// root-owned OpenRC layout. installBinary consults this before chowning a
// freshly swapped binary to the service user: chowning a file in the
// root-owned tree would hand the file itself to the service user, who could
// then chmod it writable and plant arbitrary code at a path root later
// executes (the update-time version verify, the next sudo update). The whole
// point of the root-owned tree is that root owns every file in it; the
// service user only ever needs read+exec (0755), which root ownership
// delivers.
func openrcTreeOwnedByRoot(path string) bool {
	return strings.HasPrefix(path, openrcInstallRoot+"/") ||
		path == openrcInstallRoot ||
		strings.HasPrefix(path, openrcRootToolPath)
}

// openrcStagedRootToolPath returns the root-owned staged tool path on Linux,
// or "" elsewhere. platform-crossing callers (tool_path_unix.go compiled for
// darwin too) use this instead of the linux-only const.
func openrcStagedRootToolPath() string { return openrcRootToolPath }

// openrcUpdateToolPath picks the binary the PERIODIC CRON ENTRY will execute.
//
// SECURITY: that entry runs as ROOT. Anything it executes must live somewhere
// no non-root user can influence. The install tree is normally inside the
// service user's home, and root-owning the tree does not help: the user owns
// their home, so they can rename the tree aside and recreate the path with
// their own urnet-tools - root then runs it at the next tick. The root-owned
// copy under /usr/local/libexec is therefore preferred over every other
// candidate, and a user-reachable path is only used when that copy is absent
// (a pre-upgrade install), which openrcUpdateCronScript then guards.
func openrcUpdateToolPath(p Provider) string {
	if fi, err := openrcStatFn(openrcRootToolPath); err == nil && !fi.IsDir() {
		return openrcRootToolPath
	}
	if p.Binary != "" {
		cand := filepath.Join(filepath.Dir(p.Binary), "urnet-tools")
		if fi, err := openrcStatFn(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if p, err := openrcLookPathFn("urnet-tools"); err == nil {
		return p
	}
	if self, err := openrcExecutableFn(); err == nil && self != "" {
		return self
	}
	return "urnet-tools"
}

// openrcSudoHint appends an actionable elevation hint to permission errors:
// the periodic queue lives under /etc, so a non-root run cannot manage it and
// a bare "permission denied" would leave the operator guessing.
func openrcSudoHint(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return " — re-run as root (sudo urnet-tools ...)"
	}
	return ""
}

// openrcElevationHint is the euid-based hint for rc-service/rc-update
// failures: their exit status says nothing about the cause, but the system
// service requires root, so an unprivileged failure is called out instead of
// surfacing a bare errno.
func openrcElevationHint() string {
	if os.Geteuid() != 0 {
		return " — re-run as root (sudo urnet-tools ...)"
	}
	return ""
}

// openrcCronEntryPaths lists the auto-update entry (and its installer-paused
// form) in one queue directory.
func openrcCronEntryPaths(dir string) []string {
	return []string{
		filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronName),
		filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronPausedName),
	}
}

// openrcSetAutoUpdate writes or removes the busybox crond periodic entry that
// runs `urnet-tools update -f` at the requested interval.
//
// Why /etc/periodic/<interval>/ and not a line in /etc/crontabs/root:
// Alpine's stock /etc/crontabs/root already contains the run-parts lines for
// /etc/periodic/{daily,weekly,monthly} (verified on alpine:latest), so a
// drop-in file is idempotent, needs no crontab parsing or rewriting, cannot
// race or corrupt an operator-managed crontab, and is removed with a single
// rm on `off`. The interval maps directly onto the periodic queue. crond
// itself must be running for the entry to fire; openrcCrondNote reports when
// it is not.
func openrcSetAutoUpdate(interval, toolPath string) error {
	if interval == "off" {
		for _, dir := range openrcPeriodicIntervals {
			for _, path := range openrcCronEntryPaths(dir) {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("auto-update off: remove %s: %w%s", path, err, openrcSudoHint(err))
				}
			}
		}
		return nil
	}
	if !openrcValidInterval(interval) {
		return fmt.Errorf("invalid interval %q: daily|weekly|monthly|off", interval)
	}
	// Clear every queue — the entry AND any installer-paused leftover, in the
	// target directory too — so an interval change never leaves two entries
	// firing and no stale file survives the rewrite.
	for _, dir := range openrcPeriodicIntervals {
		for _, path := range openrcCronEntryPaths(dir) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("auto-update %s: remove stale %s: %w%s", interval, path, err, openrcSudoHint(err))
			}
		}
	}
	dir := filepath.Join(openrcPeriodicBaseDir, interval)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("auto-update: create %s: %w%s", dir, err, openrcSudoHint(err))
	}
	path := filepath.Join(dir, openrcUpdateCronName)
	// run-parts executes only regular executable files; writeFileAtomic
	// applies the mode through the held fd (temp + rename), so a crash never
	// leaves a half-written entry.
	if err := writeFileAtomic(path, []byte(openrcUpdateCronScript(toolPath)), 0o755); err != nil {
		return fmt.Errorf("auto-update: write %s: %w%s", path, err, openrcSudoHint(err))
	}
	return nil
}

// openrcUpdateCronScript is the periodic entry's content. `update -f` is the
// unattended form: no confirmation prompts, and a failed restart surfaces in
// the cron output.
// shellQuoteSingle wraps a string in POSIX single quotes, escaping embedded
// single quotes. The cron entry interpolates a filesystem path into a /bin/sh
// script, so the path must survive quoting intact whatever it contains.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"\''"`) + "'"
}

func openrcUpdateCronScript(toolPath string) string {
	return "#!/bin/sh\n" +
		"# URnetwork provider auto-update — installed by `urnet-tools auto-update`.\n" +
		"# Runs from busybox crond's periodic queue (see /etc/crontabs/root), AS ROOT.\n" +
		"# Zero-downtime HotSwap is unavailable under OpenRC (no sd_notify):\n" +
		"# the update performs a stop/start service restart.\n" +
		"#\n" +
		"# SECURITY: this entry executes as root, so the binary it runs must not be\n" +
		"# reachable through any directory a non-root user can write. Root-owned\n" +
		"# FILES are not enough — a user who owns an ancestor directory can rename\n" +
		"# it aside and plant their own binary at the same path. Verify every\n" +
		"# component from the root down before exec'ing; refuse loudly otherwise.\n" +
		"tool=" + shellQuoteSingle(toolPath) + "\n" +
		"dir=$(dirname \"$tool\")\n" +
		// dirname never progresses for a bare name (dirname utool = "."), so
		// guard against the loop spinning forever on a tool path without a
		// directory component.
		"prev=\n" +
		"while [ \"$dir\" != \"/\" ] && [ -n \"$dir\" ]; do\n" +
		"	if [ \"$dir\" = \"$prev\" ]; then\n" +
		"		echo \"urnetwork-update: refusing to run $tool: cannot resolve a directory for it\" >&2; exit 1\n" +
		"	fi\n" +
		"	prev=\"$dir\"\n" +
		"	if [ ! -d \"$dir\" ]; then\n" +
		"		echo \"urnetwork-update: refusing to run: $dir is missing\" >&2; exit 1\n" +
		"	fi\n" +
		"	owner=$(stat -c %u \"$dir\" 2>/dev/null)\n" +
		"	mode=$(stat -c %a \"$dir\" 2>/dev/null)\n" +
		"\tif [ \"$owner\" != \"0\" ] || [ \"$((0$mode & 022))\" -ne 0 ]; then\n" +
		"\t\techo \"urnetwork-update: refusing to run $tool: $dir is writable by a non-root user (owner=$owner mode=$mode)\" >&2\n" +
		"\t\texit 1\n" +
		"\tfi\n" +
		"\tdir=$(dirname \"$dir\")\n" +
		"done\n" +
		"exec \"$tool\" update -f\n"
}

// openrcCrondStatusFn probes the crond service: nil when started, an
// ExitError with code 3 when stopped, any other error when the check itself
// failed. Seam for tests (the default runs rc-service with its output
// discarded).
var openrcCrondStatusFn = func() error {
	return exec.Command(openrcToolPathFn("rc-service"), "crond", "status").Run()
}

// openrcCrondNote returns an actionable note when the periodic entry would
// never fire, so `auto-update weekly` does not silently schedule into a queue
// nothing runs. Empty when crond has an OpenRC service that reports started.
func openrcCrondNote() string {
	crondInit := "/etc/init.d/crond"
	if !openrcPathExists(crondInit) {
		return "the crond OpenRC service is not installed (on Alpine: apk add busybox-openrc); the auto-update entry will not fire until crond runs"
	}
	// rc-service status exits 0 when started and 3 when stopped. The probe's
	// output is discarded so it never interleaves with this command's own
	// output, and "could not check" is distinguished from "stopped".
	err := openrcCrondStatusFn()
	if err == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
		return "crond is not started; enable it so the auto-update entry fires: rc-update add crond default && rc-service crond start"
	}
	return fmt.Sprintf("could not check whether crond is running (%v); make sure busybox crond runs or the auto-update entry will not fire", err)
}

// openrcCleanupCronEntries removes the periodic auto-update entries (and any
// installer-paused leftovers) from every queue. Split from the service
// cleanup because the entry can exist without the init script: auto-update
// does not require the service, and an uninstall must not leave crond firing
// for a provider that is gone.
func openrcCleanupCronEntries() {
	for _, dir := range openrcPeriodicIntervals {
		for _, path := range openrcCronEntryPaths(dir) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove %s: %v (run: sudo rm -f %s)\n", path, err, path)
			}
		}
	}
}

// openrcCleanupService stops and de-registers the service and removes the
// init script. Callers gate this on the service actually being installed.
func openrcCleanupService() {
	if err := openrcRunFn(openrcServiceArgv("stop")...); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: warning: rc-service %s stop: %v\n", openrcServiceName, err)
	}
	if err := openrcRunFn(openrcUpdateArgv("del")...); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: warning: rc-update del %s: %v\n", openrcServiceName, err)
	}
	if err := os.Remove(openrcInitScriptPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove %s: %v (run: sudo rm -f %s)\n", openrcInitScriptPath, err, openrcInitScriptPath)
	}
}

// openrcCleanup removes the OpenRC lifecycle artifacts for an uninstall: the
// service is stopped and de-registered, the init script is removed, and the
// periodic auto-update entry is cleared. Every step is best-effort with a
// warning: uninstall must not abort halfway on a permission error, and the
// caller may not be root (the system service needs root to remove).
//
// Used by callers that KNOW the uninstall targets the service's own provider;
// cleanupLifecycle composes the two halves selectively (see there).
func openrcCleanup() {
	openrcCleanupService()
	openrcCleanupCronEntries()
}

// providerSupervisedByOpenRC reports whether the provider process is the
// direct child of OpenRC's supervise-daemon, i.e. the process the urnetwork
// service supervises. The check reads the kernel's parent pid
// (/proc/<pid>/stat field 4) and the parent's comm — the kernel truncates
// comm to 15 bytes, so "supervise-daemon" is stored as "supervise-daemo".
// Nothing in the process's own environment or argv is trusted.
func providerSupervisedByOpenRC(p Provider) bool {
	if p.PID <= 0 {
		return false
	}
	ppid := parentPID(p.PID)
	if ppid <= 1 {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", ppid))
	if err != nil {
		return false
	}
	return isSuperviseDaemonComm(strings.TrimSpace(string(comm)))
}

// isSuperviseDaemonComm matches the kernel's comm for supervise-daemon,
// including its 15-byte truncation ("supervise-daemo" — TASK_COMM_LEN is 16
// including the NUL). Exact matches only: a prefix test would also accept
// short comms like "su", misclassifying every su-parented provider as
// OpenRC-supervised.
func isSuperviseDaemonComm(comm string) bool {
	return comm == "supervise-daemon" || comm == "supervise-daemo"
}

// parentPID returns the parent pid from /proc/<pid>/stat.
func parentPID(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	return parseParentPID(string(b))
}

// parseParentPID extracts field 4 (ppid) from /proc/<pid>/stat content. comm
// may contain spaces and parentheses, so only the fields after the LAST ')'
// are parsed (state ppid pgrp ...).
func parseParentPID(stat string) int {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// openrcRestartService restarts the OpenRC service when the running provider
// is the process supervise-daemon supervises. handled=false otherwise: a
// systemd-unit provider is never routed through OpenRC, and a bare process
// (manual launch) is not the service's — restarting the service would not
// restart it, so the caller's normal path (with its actionable error) runs.
func openrcRestartService(p Provider) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	if p.Unit != "" {
		return false, nil
	}
	if !providerSupervisedByOpenRCFn(p) {
		return false, nil
	}
	if reason := openrcInitScriptStaleReason(openrcInitScriptPath); reason != "" {
		fmt.Fprintf(os.Stderr, "warning: %s %s; rerun the installer to refresh it (a binary update does not rewrite it)\n", openrcInitScriptPath, reason)
	}
	fmt.Printf("restarting %s (OpenRC service, pid %d)...\n", openrcServiceName, p.PID)
	if err := openrcRunFn(openrcServiceArgv("restart")...); err != nil {
		return true, fmt.Errorf("rc-service %s restart: %w%s", openrcServiceName, err, openrcElevationHint())
	}
	// rc-service reports success even when the old provider survived its stop
	// (a hung or SIGTERM-ignoring process), and the restart then runs a second
	// provider with the same identity and state. Confirm the old pid is gone.
	if err := openrcWaitOldGone(p.PID); err != nil {
		return true, err
	}
	fmt.Printf("restarted %s (OpenRC service)\n", openrcServiceName)
	return true, nil
}

// openrcProcessAliveFn and openrcOldGoneWait are variables so tests can drive
// the post-restart check without a real process or real time.
var (
	openrcProcessAliveFn = processAlive
	openrcOldGoneWait    = 5 * time.Second
	openrcOldGoneStep    = 200 * time.Millisecond
)

// openrcWaitOldGone waits up to openrcOldGoneWait for the pre-restart provider
// pid to exit and returns an error naming it if it never does.
func openrcWaitOldGone(oldPid int) error {
	if oldPid <= 0 {
		return nil
	}
	deadline := time.Now().Add(openrcOldGoneWait)
	for openrcProcessAliveFn(oldPid) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("rc-service %s restart reported success but the previous provider (pid %d) is still running, so two providers may now share one identity and state; stop pid %d and re-check with 'urnet-tools status'", openrcServiceName, oldPid, oldPid)
		}
		time.Sleep(openrcOldGoneStep)
	}
	return nil
}

// openrcHotSwapDecline returns a clean decline when the running provider is
// supervised by OpenRC's supervise-daemon. An in-process baton handoff has no
// way to hand the supervised MainPID over (no sd_notify), so the successor
// would be orphaned from supervise-daemon, which would then respawn a SECOND
// provider. Declining makes the update fall back to a stop/start service
// restart, the documented OpenRC trade.
func openrcHotSwapDecline(p Provider) error {
	if !providerSupervisedByOpenRCFn(p) {
		return nil
	}
	return ErrHotSwapOpenRC
}

// openrcServiceEnabledFn reports whether the service is registered in the
// default runlevel, i.e. meant to be running after a boot. Seam for tests.
var openrcServiceEnabledFn = openrcServiceEnabled

// openrcProbeTimeout bounds the rc-update probe. It is a fast local read, so
// the bound only fires on a wedged or unresponsive OpenRC, and it is there so
// `status` can never hang on the enable check.
const openrcProbeTimeout = 5 * time.Second

// openrcServiceEnabled reads the default runlevel from rc-update. Each enabled
// service is printed as "<name> | default" (the first field is padded), so the
// line must carry the pipe and the name before it must match exactly. A probe
// that fails reports false: an UNKNOWN enable state must never be reported as
// enabled, or a deliberately stopped service would be dressed up as a fault.
func openrcServiceEnabled() bool {
	ctx, cancel := context.WithTimeout(context.Background(), openrcProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, openrcToolPathFn("rc-update"), "show", "default").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, _, ok := strings.Cut(line, "|")
		if !ok {
			continue // a banner or notice without the pipe is not a runlevel row
		}
		if strings.TrimSpace(name) == openrcServiceName {
			return true
		}
	}
	return false
}

// openrcExitStatus returns the exit code of a runOpenRCCommand result: 0 for
// nil, the process code for an ExitError, and -1 when the command could not be
// started at all. A start failure is NOT "stopped": it must not be read as a
// service fault.
func openrcExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// renderOpenRCStatus prints the service's supervisor view for `urnet-tools
// status` on an OpenRC host; the caller then falls through to the table,
// which carries the live control-socket view.
//
// rc-service status exits 0 when the service is started and 3 when it is
// stopped, and its output is passed through either way. Verified on OpenRC
// 0.63: a service that never started, one that failed to start, and one whose
// supervise-daemon exhausted its respawn limit all report "status: stopped"
// with exit 3, so 3 is the code the fault keys on. The stopped case used
// to read as a bare "stopped" whatever the reason. The two reasons differ and
// only one is a fault: a service that is NOT enabled at boot was simply left
// stopped by the operator, while one that IS enabled should be running, so it
// exited and either failed to restart or exhausted supervise-daemon's
// respawn limit (respawn_max=10, set by the installer's init script). The
// second case is surfaced loudly with the next step, because the operator
// running `status` is otherwise told nothing is wrong.
func renderOpenRCStatus() error {
	if openrcExitStatus(openrcRunFn(openrcServiceArgv("status")...)) != 3 {
		return nil
	}
	if !openrcServiceEnabledFn() {
		fmt.Fprintf(os.Stderr, "\nthe %s service is stopped and is not enabled at boot; start it with: sudo rc-service %s start\n", openrcServiceName, openrcServiceName)
		return nil
	}
	fmt.Fprintf(os.Stderr, "\nWARNING: the %s service is enabled in the default runlevel but is NOT running.\n", openrcServiceName)
	fmt.Fprintf(os.Stderr, "it exited and either failed to restart or exhausted supervise-daemon's respawn limit (respawn_max=10).\n")
	fmt.Fprintf(os.Stderr, "  start it:      sudo rc-service %s start\n", openrcServiceName)
	fmt.Fprintf(os.Stderr, "  if it recurs:  urnet-tools logs   and   rc-service %s status\n", openrcServiceName)
	return nil
}

var (
	// real directives only: a line that starts with the directive, quoted or not,
	// not a comment that mentions it
	openrcInitMarkerRe = regexp.MustCompile(`(?m)^\s*export\s+URNETWORK_INIT=["']?openrc["']?\s*$`)
	openrcRetryRe      = regexp.MustCompile(`(?m)^\s*retry=`)
)

// openrcInitScriptStaleReason says what an installed init script lacks that
// the current installer writes: the URNETWORK_INIT marker that lets the thrash
// watchdog restart under supervise-daemon, and the retry= stop schedule that
// ends a hung provider. Empty when the script has both or cannot be read.
func openrcInitScriptStaleReason(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	script := string(b)
	var missing []string
	if !openrcInitMarkerRe.MatchString(script) {
		missing = append(missing, "the URNETWORK_INIT=openrc marker (the thrash restart stays off)")
	}
	if !openrcRetryRe.MatchString(script) {
		missing = append(missing, "a retry= stop schedule (a hung provider is never killed)")
	}
	if len(missing) == 0 {
		return ""
	}
	return "predates this release and lacks " + strings.Join(missing, " and ")
}
