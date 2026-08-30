package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// defaultLogTailLines is how many lines `logs` prints before following.
const defaultLogTailLines = 250

// parseLogLineCount parses the optional trailing line-count argument; the
// default is defaultLogTailLines.
func parseLogLineCount(rest []string) (int, error) {
	if len(rest) == 0 {
		return defaultLogTailLines, nil
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid line count %q (want a positive integer)", rest[0])
	}
	return n, nil
}

func RunDocker(args []string) error {
	// Match on args[0] regardless of trailing args, as the old dispatcher did:
	// `-v junk` still prints the version (Sonnet/Muse review).
	if len(args) >= 1 {
		switch args[0] {
		case "-v", "--version":
			fmt.Println(ToolVersion)
			return nil
		case "version":
			fmt.Printf("urnet-tools %s\n", ToolVersion)
			providers := DiscoverDocker()
			if len(providers) == 0 {
				fmt.Println("  no providers discovered")
				return nil
			}
			for _, p := range providers {
				status := "running"
				if !p.Running {
					status = "stopped"
				}
				stale := ""
				if p.BinaryDeleted {
					stale = " (disk binary stale \u2014 restart needed)"
				}
				ver := p.Version
				if ver == "" {
					ver = "-"
				}
				pid := p.PID
				pidStr := fmt.Sprintf("%d", pid)
				if pid <= 0 {
					pidStr = "-"
				}
				fmt.Printf("  %s: %s (%s, pid %s)%s\n",
					providerLabel(p), ver, status, pidStr, stale)
			}
			return nil
		}
	}
	rootCmd := buildDockerRootCmd()
	if args == nil {
		args = []string{}
	}
	rootCmd.SetArgs(args)
	return rootCmd.Execute()
}

// hasHelpFlag reports whether args contains -h/--help (used by the
// read-only docker subcommands so help never reaches a delegated action).
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// usageDocker prints the urnet-docker subcommand summary.
func usageDocker() {
	fmt.Fprintf(os.Stderr, `urnet-docker — docker-container URnetwork manager

Usage: urnet-docker <command> [flags]

Core Commands:
  providers               list all provider containers (identified by in-container JWT)
  status [target]         detailed status of one container
  start|stop|restart [target]   control container lifecycle (docker start/stop/restart)
  logs [target] [N]       follow container logs (RAMLOGS-aware /dev/shm fallback)
  auth [<code>] [target]  authenticate provider inside container
  choose-network <api> <connect> [target]  set API/connect endpoints inside container
  summary [target]        activity & performance summary for container
  report <url> [target]   set hub report URL inside container (no restart)
  update [<container>]     update a container's provider in place (no recreate), or the host binary
  version                 print tool version

Proxy Management [target]:
  proxy add <file>          copy host file and bulk add proxies to container
  proxy clear|remove        remove configured proxies
  proxy refresh             hot-reload proxy sources inside container
  proxy add-source <url>    add URL proxy source
  proxy remove-source <url> remove URL proxy source
  proxy health              show dead/degraded proxy health and live event log
  proxy traffic             real-time bandwidth & client session load
  proxy remove-dead         prune dead/degraded proxies
  proxy trim <N>            hold running proxies at N, shed worst first (F -> A)
  proxy exclude [<pattern>] exclude proxies matching pattern

Performance & Tuning [target]:
  self-heal <on|off|status> manage automatic proxy self-healing
  set <key> [<value>|off]   runtime tuning override in container state
  fast-auth <on|off|status> manage auth rate limiter bypass marker

Hub & Session Management [target]:
  hub link <url> [--token]  link container provider to central hub
  hub unlink                remove hub trust and report URL
  session save <file>       export encrypted identity+proxy bundle
  session load <file>       import encrypted bundle into container

Advanced:
  exec [target] [--] <cmd...> run arbitrary command inside container; target flags
                          (--unit/--network/etc) must precede the command; use "--" to
                          forward inner flags verbatim, e.g.
                          urnet-docker exec --unit <name> -- urnet-tools proxy add --proxy_file=/tmp/p.txt

Targeting flags (required when more than one provider container exists):
  --unit <name>          container name (mapped to Unit)
  --network <name>       JWT network name, e.g. tacogonzalez3000
  --network-id <id>      JWT network id
  --state-dir <path>     state dir INSIDE the container (rarely needed)

Global flags:
  -f, --force            bypass the confirm gate (for scripts/cron)
  -n, --dry-run          show what would happen without doing it
  -h, --help             show help (never executes anything)
`)
}

// dockerTargetFromArgs reuses parseTargetFlags; container targets map the
// --unit flag to the container name. A leading bare positional that matches
// a discovered container is ALSO accepted as the target (the usage text
// documents `status [target]`, `logs [target] [N]`), so single-target
// commands work without repeating --unit. The providers list is required to
// validate the bare name.
func dockerTargetFromArgs(args []string, providers []Provider) (Target, []string, error) {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return t, rest, err
	}
	t, rest = consumeDockerBareTarget(providers, t, rest)
	return t, rest, nil
}

// consumeDockerBareTarget promotes a bare positional that matches a
// discovered container name to the target when no explicit target flag was
// given. Flags are skipped, so `proxy clear --force urnet-test` still
// resolves the container. The first non-flag positional that does NOT match
// any container is left untouched (it is a command argument, e.g. a proxy
// file path).
func consumeDockerBareTarget(providers []Provider, t Target, rest []string) (Target, []string) {
	if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
		return t, rest
	}
	for i, a := range rest {
		if strings.HasPrefix(a, "-") {
			continue
		}
		for _, p := range providers {
			if p.Unit == a {
				t.Unit = a
				out := append([]string{}, rest[:i]...)
				out = append(out, rest[i+1:]...)
				return t, out
			}
		}
		return t, rest // first non-flag positional doesn't match a container
	}
	return t, rest
}

// cmdDockerProviders lists every provider container on the box.
func cmdDockerProviders(args []string) error {
	providers := DiscoverDocker()
	if len(providers) == 0 {
		fmt.Println("no provider containers found")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CONTAINER\tNETWORK\tSTATE-DIR(in)\tIMAGE\tRUNNING")
	for _, p := range providers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n",
			p.Unit, p.Network, p.StateDir, p.Binary, p.Running)
	}
	return w.Flush()
}

// cmdDockerStatus shows details for one container.
func cmdDockerStatus(args []string) error {
	providers := DiscoverDocker()
	t, _, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "container:\t%s\n", p.Unit)
	fmt.Fprintf(w, "image:\t%s\n", p.Binary)
	fmt.Fprintf(w, "running:\t%v\n", p.Running)
	fmt.Fprintf(w, "state-dir (in container):\t%s\n", p.StateDir)
	fmt.Fprintf(w, "network:\t%s\n", p.Network)
	fmt.Fprintf(w, "network-id:\t%s\n", p.NetworkID)
	if !p.JWTExpires.IsZero() {
		fmt.Fprintf(w, "jwt-expires:\t%s\n", p.JWTExpires.Format("2006-01-02 15:04:05"))
	}
	return w.Flush()
}

// cmdDockerExec runs a command inside the targeted container — the
// delegation path (e.g. `urnet-docker exec urnet-tools proxy add ...`).
// Target flags come BEFORE the command; everything from the first
// positional onward is the in-container command and must pass through
// verbatim, including its own --flags (opus5 F1: strict parsing rejected
// `--proxy_file=` before delegation).
func cmdDockerExec(args []string) error {
	// Split at the first non-flag token: target flags before it, command
	// after it. A `--` separator forwards everything after it VERBATIM to
	// the container command (standard `--` separator convention) so inner-command
	// flags like -f or --verbose can never be mistaken for urnet-docker
	// flags or silently dropped.
	pre, rest, err := splitExecArgs(args)
	if err == errHelpShown {
		// Print the usage on pre-separator help — exiting silently on
		// `exec --unit x --help` reads as a no-op, not documentation
		// (Sonnet final review MEDIUM).
		usageDocker()
		return nil
	}
	if err != nil {
		return err
	}
	t, _, err := parseTargetFlags(pre)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("exec requires a command, e.g. 'urnet-docker exec -- urnet-tools proxy add --proxy_file=/tmp/p.txt'")
	}
	providers := DiscoverDocker()
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	// p.Unit holds the container name; delegate the command verbatim.
	return containerExecByName(p.Unit, rest...)
}

// splitExecArgs divides exec arguments into the pre-command urnet-docker
// targeting flags and the verbatim in-container command. A `--` separator
// puts EVERYTHING after it into the command (standard `--` separator convention);
// without it, the command starts at the first non-flag token. Unknown
// leading flags are refused (never silently dropped) with a hint to use --.
func splitExecArgs(args []string) (pre, rest []string, err error) {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep >= 0 {
		return args[:sep], args[sep+1:], nil
	}
	split := 0
	for split < len(args) && strings.HasPrefix(args[split], "-") {
		switch args[split] {
		case "--unit", "--user", "--network", "--network-id", "--state-dir":
			// A recognized target flag MUST have a value; a trailing flag
			// (nothing after it) would push split past len(args) and panic
			// on the slice below (coderabbit critical).
			if split+1 >= len(args) {
				return nil, nil, fmt.Errorf("target flag %q requires a value (e.g. %q <name>)", args[split], args[split])
			}
			split += 2 // flag + value
		case "-h", "--help":
			// Belt-and-suspenders: RunDocker already handles -h/--help via
			// hasHelpFlag before dispatching, but keep it here so a direct
			// call never misroutes help into a delegated action.
			return nil, nil, errHelpShown
		default:
			// Unknown leading flag (only --unit/--user/--network/
			// --network-id/--state-dir and -h/--help are recognized):
			// refuse rather than silently drop it
			// (the rewrite's own philosophy — a flag that vanishes can
			// mask a real action). Suggest the -- separator.
			return nil, nil, fmt.Errorf("unknown flag %q before exec command — use `--` to pass flags to the container command, e.g. 'urnet-docker exec --unit <name> -- <cmd> -f'", args[split])
		}
	}
	return args[:split], args[split:], nil
}

// cmdDockerUpdate updates the urnet-docker binary on the host (no target) or
// the provider inside a running container in place (with a target), without
// recreating the container. The in-container path runs the container's own
// urnet-tools self-update via docker exec.
func cmdDockerUpdate(args []string, force, dryRun bool) error {
	providers := DiscoverDocker()
	t, rest, err := updateTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	if t.Unit == "" && t.User == "" && t.Network == "" && t.NetworkID == "" && t.StateDir == "" {
		// No container target resolved (LA1 defect 1, 2026-08-24): bare
		// `update` must NOT silently become a host self-update when
		// provider containers exist. With exactly ONE container, target
		// it; with several, refuse and list them; with none, fall through
		// to the host self-update (nothing else to update).
		//
		// Host self-update pinning options (--tag/--digest/--url) say the
		// user asked to update the HOST tool, not a container. Route those
		// to the host self-update so the options take effect, never to the
		// lone-container auto-select below (CodeRabbit PR #465).
		if hasSelfUpdateArg(args) {
			return cmdSelfUpdate(args, force, dryRun)
		}
		switch len(providers) {
		case 0:
			return cmdSelfUpdate(args, force, dryRun)
		case 1:
			t = Target{Unit: providers[0].Unit}
			fmt.Printf("no target given; updating the lone provider container %s (use `self-update` for the host tool)\n", providers[0].Unit)
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "%d provider containers found — name one (e.g. `update <container>`), or use `self-update` for the host tool:\n", len(providers))
			for _, p := range providers {
				fmt.Fprintf(&b, "  %s\n", p.Unit)
			}
			return fmt.Errorf("%s", b.String())
		}
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument(s) after update target: %v", rest)
	}
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if !p.Running {
		return fmt.Errorf("container %s is not running; start it before updating in place", p.Unit)
	}
	ok, err := confirmGate("update provider inside container "+p.Unit+" in place (no recreate)", p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run or declined
	}
	// Older container images ship a broken in-place update routine (busybox
	// mktemp rejects the XXXX.tar.gz template, and pkill -x misses the
	// 15-char-truncated process comm). Repair that routine from the host first
	// so in-place update works on ANY container image, not just ones that
	// already ship the fixed script. Idempotent: it only rewrites the known
	// broken patterns to their fixed forms.
	if err := repairContainerUpdateScript(p.Unit); err != nil {
		return fmt.Errorf("prepare %s for in-place update: %w", p.Unit, err)
	}
	// Read the PRE-update live version BEFORE the in-place swap. After the
	// swap the on-disk binary is already the new version, so capturing the
	// baseline here (post-swap) would make the difference check below
	// unsatisfiable — every real update would falsely fail and force an
	// unplanned container cycle (ox-alpha C1, PR #465). Read it first so
	// waitForLiveVersion can detect the bump to the new release.
	beforeVer := strings.TrimSpace(containerLiveVersion(p.Unit))
	fmt.Printf("updating provider inside %s in place (urnet-tools update)...\n", p.Unit)
	if err := containerExecByName(p.Unit, "urnet-tools", "update"); err != nil {
		return err
	}
	// Older container images stop when the provider process is killed (their
	// start loop exits instead of relaunching). Bring the SAME container back
	// up (no recreate) so the provider launches on the newly-swapped binary.
	// This is what docker start does; verified live on an old 26.4-image
	// container: after the swap the container stopped, and docker start brought
	// it up running the new version, container ID unchanged.
	if !containerRunning(p.Unit) {
		fmt.Printf("container %s stopped after the swap; starting it (no recreate)...\n", p.Unit)
		if err := containerStartByName(p.Unit); err != nil {
			return fmt.Errorf("restart container %s after update: %w", p.Unit, err)
		}
	}
	if beforeVer == "" {
		// Could not read the running binary's version pre-swap (the image's
		// --version output is empty/unparseable). Fall back to confirming the
		// provider process is up after the swap. Poll through the same
		// recovery window as waitForLiveVersion below instead of a single
		// check: a freshly started container takes a few seconds to boot its
		// provider, and one immediate probe reported a false "could not
		// confirm" (coderabbit follow-up, PR #10).
		for i := 0; i < 60; i++ {
			if containerProviderAlive(p.Unit) {
				fmt.Printf("update applied to %s (could not read pre-update version; provider process is up).\n", p.Unit)
				return nil
			}
			time.Sleep(1 * time.Second)
		}
		return fmt.Errorf("could not confirm the provider process inside %s after the update — check `docker logs %s`", p.Unit, p.Unit)
	}
	// No-op: the in-container update finished but the live version is
	// unchanged (the container already ran the target release). Report it and
	// stop — do not bounce a healthy production container or fail hard
	// (ox-alpha M2, PR #465).
	if cur := strings.TrimSpace(containerLiveVersion(p.Unit)); cur == beforeVer {
		fmt.Printf("%s is already at %s (no change).\n", p.Unit, cur)
		return nil
	}
	if v, ok := waitForLiveVersion(p.Unit, beforeVer, 60); ok {
		fmt.Printf("verified: %s now runs %s (was %s).\n", p.Unit, v, beforeVer)
		return nil
	}
	// The swap moved the on-disk binary to a new version but the provider did
	// not come back up on it within the window; cycle the container once and
	// re-verify before declaring failure.
	fmt.Printf("provider did not come back on a new version; cycling container %s once...\n", p.Unit)
	if err := containerRestartByName(p.Unit); err != nil {
		return fmt.Errorf("cycle container %s after update: %w", p.Unit, err)
	}
	if v, ok := waitForLiveVersion(p.Unit, beforeVer, 60); ok {
		fmt.Printf("verified: %s now runs %s (after one cycle).\n", p.Unit, v)
		return nil
	}
	return fmt.Errorf("container %s did not come up on a new version within the wait window — check `docker logs %s`", p.Unit, p.Unit)
}

// containerProviderAlive reports whether a provider process is running
// inside the named container (pgrep via docker exec). Declared as a var
// (not a func) so tests can inject a mock (CR #1).
var containerProviderAlive = func(name string) bool {
	cmd := exec.Command(dockerCLI(), "exec", name, "sh", "-c",
		"pgrep -f 'urnetwork_.*_stable provide' >/dev/null 2>&1")
	return cmd.Run() == nil
}

// containerLiveVersion reads the provider binary's --version inside the
// container. Best effort: empty string when unreadable. Declared as a var
// (not a func) so tests can inject a mock (CR #1).
var containerLiveVersion = func(name string) string {
	cmd := exec.Command(dockerCLI(), "exec", name, "sh", "-c",
		"for b in /app/urnetwork_*_stable; do [ -x \"$b\" ] && \"$b\" --version 2>/dev/null && break; done")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// containerRunning reports whether the named container is currently running.
// Declared as a var (not a func) so tests can inject a mock (CR #1).
var containerRunning = func(name string) bool {
	cmd := exec.Command(dockerCLI(), "inspect", "-f", "{{.State.Running}}", name)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// waitForLiveVersion polls until the container's live provider version
// DIFFERS from prev (i.e. an in-place binary swap actually landed a new
// version) or until timeout. Returns the observed version on success.
//
// It compares against the PRE-update live reading, NOT the docker image tag
// (p.Version): the image tag is fixed at container-creation time and never
// changes on an in-place binary swap, while the in-container update picks
// its own target via latestRelease() and never reports it back. Comparing
// against the tag produced a false failure on a real update and a false
// success on a no-op update (CR #1). A no-op update (same version) will now
// correctly report failure rather than a false success.
func waitForLiveVersion(name, prev string, timeoutSec int) (string, bool) {
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for {
		if containerRunning(name) && containerProviderAlive(name) {
			if v := containerLiveVersion(name); v != "" && v != prev {
				return v, true
			}
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(3 * time.Second)
	}
}

// repairContainerUpdateScript applies two safe, idempotent fixes to a
// container's in-place update routine (/app/urnet-tools.sh) so it works on any
// image, including ones built before the fixes landed upstream:
//  1. busybox mktemp: the template must END in X, so a trailing ".tar.gz"
//     suffix fails with "Invalid argument". The tarball path is rewritten to
//     the mktemp-valid form.
//  2. pkill comm truncation: Linux truncates a process's comm to 15 chars, so
//     `pkill -x "urnetwork_<arch>_stable"` matches nothing. It is replaced with
//     `pkill -f "^/app/urnetwork_<arch>_stable provide"` (full command line).
//
// sed is invoked directly via exec.Command (no host or container /bin/sh layer),
// so the literal ${arch} is passed through untampered; only sed's own \$ escape
// is used to match a literal dollar sign.
func repairContainerUpdateScript(unit string) error {
	expr1 := "s|mktemp /tmp/urnetwork-update-XXXXXX.tar.gz|mktemp /tmp/urnetwork-update-XXXXXX|"
	expr2 := `s|pkill -x "urnetwork_\${arch}_stable"|pkill -f "^/app/urnetwork_\${arch}_stable provide"|`
	c := exec.Command(dockerCLI(), "exec", unit, "sed", "-i", expr1, "/app/urnet-tools.sh")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("repair mktemp in %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	c2 := exec.Command(dockerCLI(), "exec", unit, "sed", "-i", expr2, "/app/urnet-tools.sh")
	if out, err := c2.CombinedOutput(); err != nil {
		return fmt.Errorf("repair pkill in %s: %w (%s)", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// updateTargetFromArgs resolves a container target for `update` from either an
// explicit target flag (--unit/--user/--network/--network-id/--state-dir, bare
// or = form) or a BARE container name that exactly matches a discovered
// container (so `update ps` works just like `status ps` / `logs ps`). When no
// target resolves it returns an empty Target, and the caller falls through to
// the host self-update. This preserves host self-update args (--tag/--digest/
// --url) because those are never target flags and never match a container name.
func updateTargetFromArgs(args []string, providers []Provider) (Target, []string, error) {
	if hasAnyTargetFlag(args) {
		t, rest, err := dockerTargetFromArgs(args, providers)
		if err != nil {
			return t, rest, err
		}
		return t, rest, nil
	}
	// Host self-update pinning (--tag/--digest/--url) means the user asked to
	// update the HOST tool. Its value may coincidentally equal a container
	// name; never treat it as a container target, and return empty so
	// cmdDockerUpdate routes to the host self-update (ox-alpha L2, PR #465).
	if hasSelfUpdateArg(args) {
		return Target{}, nil, nil
	}
	// A bare container name is accepted as the target ONLY as the first
	// positional, and only when it exactly matches a discovered container. The
	// remaining arguments are returned so the caller can validate them rather
	// than silently dropping them (e.g. `update urnet-test extra`).
	first := -1
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		first = i
		break
	}
	if first >= 0 {
		for _, p := range providers {
			if p.Unit == args[first] {
				rest := append([]string{}, args[:first]...)
				rest = append(rest, args[first+1:]...)
				return Target{Unit: p.Unit}, rest, nil
			}
		}
		// A bare container name was given but matches nothing. Refuse instead
		// of silently falling through to the lone-container auto-select, which
		// would update a DIFFERENT container than the one named (ox-alpha H1,
		// PR #465).
		names := make([]string, 0, len(providers))
		for _, p := range providers {
			names = append(names, p.Unit)
		}
		sort.Strings(names)
		return Target{}, nil, fmt.Errorf("no provider container named %q — available: %s (use `self-update` for the host tool)", args[first], strings.Join(names, ", "))
	}
	// No bare container target: fall through to the host self-update and let it
	// interpret the args (remaining flags or a typo).
	return Target{}, nil, nil
}

// hasAnyTargetFlag reports whether args contain an explicit targeting flag, in
// either the bare (`--unit x`) or the `--flag=value` form. Without this, the
// `--flag=value` form would miss the in-container gate and fall through to the
// host self-update (Sonnet HIGH on #453).
func hasAnyTargetFlag(args []string) bool {
	for _, a := range args {
		if a == "--unit" || a == "--user" || a == "--network" || a == "--network-id" || a == "--state-dir" {
			return true
		}
		if strings.HasPrefix(a, "--unit=") || strings.HasPrefix(a, "--user=") ||
			strings.HasPrefix(a, "--network=") || strings.HasPrefix(a, "--network-id=") ||
			strings.HasPrefix(a, "--state-dir=") {
			return true
		}
	}
	return false
}

// hasSelfUpdateArg reports whether args contain a host self-update pinning
// option (--tag/--digest/--url), in either the bare (`--tag X`) or
// `--flag=value` form. When present, an update with no container target must
// be treated as a HOST self-update, never auto-targeted to a lone container
// (a regression introduced by the D1 auto-select: `update --tag vX` on a
// single-container box silently updated the container and dropped --tag).
func hasSelfUpdateArg(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--tag" || a == "--digest" || a == "--url":
			return true
		case strings.HasPrefix(a, "--tag=") || strings.HasPrefix(a, "--digest=") || strings.HasPrefix(a, "--url="):
			return true
		}
	}
	return false
}

// cmdDockerStart starts a stopped container (confirm gate applies).
func cmdDockerStart(args []string, force, dryRun bool) error {
	providers := DiscoverDocker()
	t, _, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if p.Running {
		fmt.Printf("container %s is already running\n", p.Unit)
		return nil
	}
	ok, err := confirmGate("start container "+p.Unit, p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}
	return containerStartByName(p.Unit)
}

// cmdDockerStop stops a running container (confirm gate applies).
func cmdDockerStop(args []string, force, dryRun bool) error {
	providers := DiscoverDocker()
	t, _, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if !p.Running {
		fmt.Printf("container %s is already stopped\n", p.Unit)
		return nil
	}
	ok, err := confirmGate("stop container "+p.Unit, p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}
	return containerStopByName(p.Unit)
}

// cmdDockerRestart restarts a container (destructive gate applies).
func cmdDockerRestart(args []string, force, dryRun bool) error {
	providers := DiscoverDocker()
	t, _, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	ok, err := confirmGate("restart container "+p.Unit, p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}
	return containerRestartByName(p.Unit)
}

// cmdDockerLogs tails logs for the targeted container: the last N lines
// (default 250), then follow. When the container runs with URNETWORK_RAMLOGS
// this streams /dev/shm/urnetwork.log via `docker exec <name> tail -n N -f`;
// otherwise it falls back to `docker logs --tail N -f`. Multiple provider
// containers with no target pop the interactive picker.
func cmdDockerLogs(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	n, err := parseLogLineCount(rest)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	// Prefer the RAMLOG file when the container runs with URNETWORK_RAMLOGS.
	if containerFileNonEmpty(p.Unit, "/dev/shm/urnetwork.log") {
		return containerFollowFile(p.Unit, "/dev/shm/urnetwork.log", n)
	}
	return containerLogsFollow(p.Unit, n)
}

// cmdDockerAuth delegates provider authentication into the container.
func cmdDockerAuth(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "auth"}, rest...)
	return containerInteractiveExecByName(p.Unit, inner...)
}

// cmdDockerChooseNetwork delegates choose_network into the container.
func cmdDockerChooseNetwork(args []string) error {
	providers := DiscoverDocker()
	// Lenient parse so pass-through flags (e.g. --reset) survive and are
	// forwarded to the container command (regression fix).
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	t, rest = consumeDockerBareTarget(providers, t, rest)
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "choose_network"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerSummary shows provider performance & activity summary.
func cmdDockerSummary(args []string) error {
	providers := DiscoverDocker()
	t, _, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	return containerExecByName(p.Unit, "urnet-tools", "proxy", "summary")
}

// cmdDockerReport configures the hub report URL inside the container.
func cmdDockerReport(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "report"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerSelfHeal manages the proxy self-heal marker inside the container.
func cmdDockerSelfHeal(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "self-heal"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerSet manages runtime tuning overrides in the container state dir.
func cmdDockerSet(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "set"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerFastAuth manages the auth rate limiter bypass marker in the container.
func cmdDockerFastAuth(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "fast-auth"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerHub delegates hub management commands into the container.
func cmdDockerHub(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "hub"}, rest...)
	return containerExecByName(p.Unit, inner...)
}

// cmdDockerSession delegates interactive session save/load into the container.
func cmdDockerSession(args []string) error {
	providers := DiscoverDocker()
	t, rest, err := dockerTargetFromArgs(args, providers)
	if err != nil {
		return err
	}
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	inner := append([]string{"urnet-tools", "session"}, rest...)
	return containerInteractiveExecByName(p.Unit, inner...)
}

// cmdDockerProxy implements host-side proxy management for containerized
// providers (Design 2). The user runs e.g. `urnet-docker proxy add ~/p.txt`
// and the exec plumbing is hidden: target resolution (interactive when
// multiple containers), host-file copy into the container, and the
// in-container urnet-tools proxy invocation.
func cmdDockerProxy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("proxy requires a subcommand: add <file> | clear | remove | add-source <url> | remove-source <url> | refresh | remove-dead | health | traffic | summary | trim <N> | exclude")
	}
	sub := args[0]
	rest := args[1:]

	// Parse target flags (may appear before or after the subcommand).
	// Use a lenient split: target flags are --unit/--user/--network/etc.
	t, rest2, err := parseTargetFlagsLenient(rest)
	if err != nil {
		return err
	}

	providers := DiscoverDocker()
	// Accept a bare container name as the target (e.g.
	// `urnet-docker proxy refresh urnet-test`) — the usage text documents
	// `[target]` for the single-target commands, and the workflows call
	// proxy subcommands with the container name as a positional. The bare
	// name must match a discovered container; a proxy file path or URL is
	// left untouched.
	t, rest2 = consumeDockerBareTarget(providers, t, rest2)
	p, err := selectTargetInteractive(providers, t)
	if err != nil {
		return err
	}
	container := p.Unit // container name

	switch sub {
	case "add":
		// Exactly one positional: the host proxy file. A leading flag (e.g.
		// --force) would be misread as a filename, so reject anything that is
		// not a single non-flag argument (DeepSeek MF2 + SF3).
		if len(rest2) != 1 || strings.HasPrefix(rest2[0], "-") {
			return fmt.Errorf("proxy add requires exactly one proxy file, e.g. 'urnet-docker proxy add ~/proxies.txt'")
		}
		hostFile := rest2[0]
		// Unique in-container path so concurrent proxy ops cannot collide
		// (DeepSeek SF4).
		inPath := fmt.Sprintf("/tmp/urnet-proxies-%d.txt", os.Getpid())
		if err := dockerCopyInto(container, hostFile, inPath); err != nil {
			return fmt.Errorf("copy %s into container: %w", hostFile, err)
		}
		defer func() {
			_ = exec.Command(dockerCLI(), "exec", container, "rm", "-f", inPath).Run()
		}()
		// --proxy_file= is REQUIRED: the in-container urnet-tools is the
		// shell wrapper (urnet-tools.sh), which forwards a bare path as a
		// key_address — the path string would be registered as a proxy
		// address instead of the file contents. --proxy_file= flows through
		// the wrapper to `provider proxy add` which reads the file.
		return containerExecByName(container, "urnet-tools", "proxy", "add", "--proxy_file="+inPath)
	case "clear":
		// Forward remaining args (e.g. --force) so clear is scriptable from
		// CI/cron on a non-TTY (DeepSeek MF1).
		inner := append([]string{"urnet-tools", "proxy", "clear"}, rest2...)
		return containerExecByName(container, inner...)
	case "remove":
		// Forward remaining args (e.g. --all, or specific proxies).
		inner := append([]string{"urnet-tools", "proxy", "remove"}, rest2...)
		return containerExecByName(container, inner...)
	case "add-source":
		if len(rest2) == 0 {
			return fmt.Errorf("proxy add-source requires a URL")
		}
		inner := append([]string{"urnet-tools", "proxy", "add-source"}, rest2...)
		return containerExecByName(container, inner...)
	case "remove-source":
		if len(rest2) == 0 {
			return fmt.Errorf("proxy remove-source requires a URL")
		}
		inner := append([]string{"urnet-tools", "proxy", "remove-source"}, rest2...)
		return containerExecByName(container, inner...)
	case "refresh":
		inner := append([]string{"urnet-tools", "proxy", "refresh"}, rest2...)
		return containerExecByName(container, inner...)
	case "remove-dead":
		inner := append([]string{"urnet-tools", "proxy", "remove-dead"}, rest2...)
		return containerExecByName(container, inner...)
	case "health":
		inner := append([]string{"urnet-tools", "proxy", "health"}, rest2...)
		return containerExecByName(container, inner...)
	case "traffic":
		inner := append([]string{"urnet-tools", "proxy", "traffic"}, rest2...)
		return containerExecByName(container, inner...)
	case "summary":
		inner := append([]string{"urnet-tools", "proxy", "summary"}, rest2...)
		return containerExecByName(container, inner...)
	case "trim":
		if len(rest2) == 0 {
			return fmt.Errorf("proxy trim requires a count (e.g. 'urnet-docker proxy trim 500')")
		}
		inner := append([]string{"urnet-tools", "proxy", "trim"}, rest2...)
		return containerExecByName(container, inner...)
	case "exclude":
		inner := append([]string{"urnet-tools", "proxy", "exclude"}, rest2...)
		return containerExecByName(container, inner...)
	default:
		return fmt.Errorf("unknown proxy subcommand %q", sub)
	}
}

// dockerCopyInto copies a host file into the container at destPath using
// `docker cp`. The host file is passed as the source; the container path is
// caller-chosen (the proxy add path uses a unique per-PID name).
func dockerCopyInto(container, hostFile, destPath string) error {
	cmd := exec.Command(dockerCLI(), "cp", hostFile, container+":"+destPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker cp: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
