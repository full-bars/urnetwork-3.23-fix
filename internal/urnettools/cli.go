package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// ToolVersion is the build-stamped tool version, wired from main.Version by
// cmd/urnet-tools and cmd/urnet-docker at startup. Default "dev" for
// un-stamped builds. Printed by the version command.
var ToolVersion = "dev"

// Run is the CLI entry point. It parses subcommands and dispatches. The
// legacy shell dispatcher bug — `--help` executing a destructive op — cannot
// exist here because every subcommand parses its own flags before acting.
func Run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	op := args[0]
	switch op {
	case "version", "--version", "-v":
		fmt.Println(ToolVersion)
		return nil
	}
	rest := args[1:]
	switch op {
	case "providers", "list", "ps":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		_, _, _ = force, dryRun, rest2 // providers is read-only; flags consumed for help handling
		return cmdProviders(rest2)
	case "status":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		_, _, _ = force, dryRun, rest2
		return cmdStatus(rest2)
	case "update":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdUpdate(rest2, force, dryRun)
	case "self-update", "selfupdate":
		// Tool-only update: refresh the urnet-tools binary itself without
		// touching any provider. Uses the same release resolution + digest
		// verification as `update`'s self-update leg, but never restarts or
		// swaps providers. Works on boxes with zero providers (e.g. a
		// docker-managed host where only the tool is installed).
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdSelfUpdate(rest2, force, dryRun)
	case "proxy":
		// Let `proxy -h/--help` and `proxy <sub> ... -h/--help` reach
		// cmdProxy's proxy-specific help instead of being intercepted as
		// global help (gauntlet finding BUG-2: nested help showed root
		// usage). Help at ANY position in the proxy args wants proxy help —
		// parseGlobalFlags scans all positions, so the interception must
		// too, else `proxy add <file> --help` still falls through to root
		// usage (Sonnet review finding).
		for _, a := range rest {
			if a == "-h" || a == "--help" {
				return cmdProxy(rest, false, false)
			}
		}
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdProxy(rest2, force, dryRun)
	case "summary":
		rest2, herr := parseDelegationArgs(rest)
		if herr == errHelpShown {
			return nil // help printed, never executes
		}
		if herr != nil {
			return herr
		}
		return cmdSimpleDelegation("summary", rest2)
	case "report":
		rest2, herr := parseDelegationArgs(rest)
		if herr == errHelpShown {
			return nil // help printed, never executes
		}
		if herr != nil {
			return herr
		}
		return cmdReport(rest2)
	case "hot-restart", "hotrestart":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil // help printed, never executes
		}
		if err != nil {
			return err
		}
		return cmdHotRestart(rest2, force, dryRun)
	case "start":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdStart(rest2, force, dryRun)
	case "stop":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdStop(rest2, force, dryRun)
	case "restart":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdRestart(rest2, force, dryRun)
	case "logs":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = force, dryRun
		return cmdLogs(rest2)
	case "hub":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdHub(rest2, force, dryRun)
	case "turbo", "eco", "lowmode", "ramlogs", "auto":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdTune(op, rest2, force, dryRun)
	case "optimize":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdOptimize(rest2, force, dryRun)
	case "auto-start", "autostart":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdAutoStart(rest2, force, dryRun)
	case "auto-update", "autoupdate":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdAutoUpdate(rest2, force, dryRun)
	case "uninstall":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdUninstall(rest2, force, dryRun)
	case "reinstall":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdReinstall(rest2, force, dryRun)
	case "session":
		// session (save/load) does NOT run parseGlobalFlags: the provider
		// -f/--force on load must reach cmdSession (same pattern as auth).
		return cmdSession(rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (see 'urnet-tools help')", op)
	}
}

// parseGlobalFlags extracts -f/--force and -n/--dry-run, returning the
// remaining args. These must be parsed before subcommand-specific flags so
// the confirm gate works uniformly.
func parseGlobalFlags(args []string) (force, dryRun bool, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--force":
			force = true
		case "-n", "--dry-run":
			dryRun = true
		case "-h", "--help":
			usage()
			return false, false, nil, errHelpShown
		default:
			rest = append(rest, args[i])
		}
	}
	return force, dryRun, rest, nil
}

// cmdSimpleDelegation handles the pass-through commands (summary):
// resolve the targeted provider, then delegate the exact subcommand to that
// provider's binary. report and hot-restart have real implementations (see
// cmdReport / cmdHotRestart) because the provider binary has no report or
// hot-restart subcommands — delegating to it printed the provider's auth
// usage and did nothing (gauntlet findings BUG-4).
func cmdSimpleDelegation(sub string, args []string) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, narrowed, err := selectTargetOrSoleAccessible(providers, t)
	if err != nil {
		return err
	}
	if narrowed {
		printNarrowedNote(len(providers), p, sub)
	}
	// The provider nests summary under the proxy branch: `provider proxy
	// summary`, not `provider summary` (gauntlet finding BUG-5). Build the
	// nested argv; everything else stays flat.
	cmdArgs := append([]string{"proxy", sub}, rest...)
	return providerSubcommand(p, cmdArgs...)
}

// cmdReport implements `urnet-tools report <url> [target]`: it writes the
// hub-report URL override file (~/.urnetwork/report_url) in the provider's
// state dir. The provider's bandwidth reporter re-reads that file every
// tick, so the change takes effect without a restart. The provider binary
// has NO report subcommand — delegating to it printed auth usage and did
// nothing (gauntlet finding BUG-4).
func cmdReport(args []string) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("report requires a URL (use 'report off' to disable)")
	}
	url := rest[0]
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	if err := writeReportURL(p, url); err != nil {
		return err
	}
	fmt.Printf("report URL for %s set to %q (effective on the reporter's next tick)\n", providerLabel(p), url)
	return nil
}

// writeReportURL writes the hub-report override file for a provider.
// Extracted from cmdReport so the write itself is directly testable with a
// Provider struct (no live process needed). The provider's bandwidth
// reporter re-reads this file every tick.
func writeReportURL(p Provider, url string) error {
	path := filepath.Join(p.StateDir, "report_url")
	// 0o644, not 0o600: urnet-tools frequently runs as a different user than
	// the provider (root tool + urnetwork-beta service — the fleet norm this
	// codebase supports via -M <user>@ and HOME= overrides). A 0600 file
	// owned by the tool's user would be unreadable by the provider process,
	// so the change would silently never take effect (Sonnet review
	// finding). The sibling override-writers use 0o644 for this reason.
	if err := os.WriteFile(path, []byte(url+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	return nil
}

// cmdHotRestart implements `urnet-tools hot-restart [target]`: it restarts
// the provider's systemd unit. The provider binary has NO hot-restart
// subcommand — its hot-restart behavior is a config/env toggle
// (URNETWORK_HOT_RESTART), not a CLI op. Delegating to the provider printed
// auth usage and did nothing (gauntlet finding BUG-4). A confirmation gate
// mirrors cmdRestart: restarting a provider is a production action and must
// not happen without --force or an explicit "yes" (Sonnet review finding —
// the original fix restarted unconditionally).
func cmdHotRestart(args []string, force, dryRun bool) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("hot-restart takes no arguments (got %v)", rest)
	}
	ok, err := confirmGate("restart "+p.Unit, p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}
	return unitCommand(p, "restart")
}

// parseDelegationArgs guards -h/--help for the pass-through commands
// (summary, report, hot-restart) BEFORE any targeting runs: those commands
// delegate to the provider binary, so without this guard `--help` would be
// forwarded and the operation would actually run (the help-never-executes
// invariant, review finding C1 class). Returns errHelpShown when help was
// printed; the caller must NOT proceed.
func parseDelegationArgs(args []string) ([]string, error) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			usage()
			return nil, errHelpShown
		}
	}
	return args, nil
}

// errHelpShown is a sentinel: help was printed, not an error condition.
var errHelpShown = fmt.Errorf("help shown")

// usage prints the subcommand summary. It is deliberately self-contained:
// an operator should be able to figure out targeting + force rules from
// this alone, without the README/wiki.
func usage() {
	fmt.Fprintf(os.Stderr, `urnet-tools — provider-aware URnetwork manager

Usage: urnet-tools <command> [flags]

Core Commands:
  providers               list all providers on this box (all users, JWT identity)
  status [target]         detailed status of one provider
  start|stop|restart [target]   control the provider's systemd unit
  update [target]         update provider(s) to latest (interactive; --tag to pin)
  self-update             update this tool binary itself (no providers touched)
  logs [target] [N]       show recent provider logs (N lines, default 250)
  summary [target]        fleet-style summary for one provider
  version                 print this tool's version

Performance & Tuning (single target; on|off unless noted):
  turbo <v4|v8|off> [target]   raise throughput limits for RAM-rich boxes
  eco <on|off> [target]        GC-tuned profile for low-RAM systems
  lowmode <on|off> [target]    reduced buffers for max RAM savings
  auto <on|off> [target]       auto-detect hardware and pick best profile
  ramlogs <on|off> [target]    zero disk I/O logging
  optimize [target]            apply golden-fleet OS/kernel limits
  hot-restart [target]         restart one provider's unit, reusing client_ids

Proxy Management [target]:
  proxy add <file>          bulk add proxies from a text file
  proxy clear|remove        remove all configured proxies
  proxy refresh             re-read configs and hot-reload proxies
  proxy add-source <url>    add a URL proxy source
  proxy remove-source <url> remove a URL proxy source
  proxy health              show dead/degraded proxies + live event log
  proxy traffic             real-time bandwidth & client session load
  proxy remove-dead         interactively prune dead/degraded/failing
  report <url> [target]     set hub report URL at runtime (no restart)

Hub Management [target]:
  hub set <host:port>       configure hub report URL
  hub off                   stop reporting to hub (no restart)
  hub install [--tag=TAG]   install hub as a systemd service

Maintenance [target]:
  reinstall                reinstall provider
  session save|load <file> [target]   export/import encrypted identity+proxy state
  uninstall                uninstall provider
  auto-update <on|off>     manage auto-update schedule
  auto-start <on|off>      toggle auto-start on login

Providers are identified three ways (use any; the = form works too,
e.g. --user=urnet is the same as --user urnet):
  --unit <name>          systemd unit, e.g. urnetwork-native.service
  --user <user>          OS user, e.g. urnet
  --network <name>       JWT network name (account identity), e.g. tacogonzalez3000
  --network-id <id>      JWT network id — TRUE unique identity; use when two
                         providers share the same network name (e.g. mainnet
                         + beta copies of one account)

Targeting rules:
  - one provider on box: no flag needed, it is used automatically
  - multiple providers: MUST pick one (--unit/--user/--network), else REFUSED
  - same network name on two providers: add --network-id or --unit to break the tie
  - batch: --include a,b (exactly these) / --exclude a,b (subtract) / --all (everything)
  - --select             interactive picker (choose A B C, skip D)
  - see 'providers' first to learn each provider's unit/user/network

Force (machines/scripts):
  -f, --force            skip confirm prompts ONLY — never picks providers.
                         Multi-provider box + -f alone = REFUSED (no target).
                         Use -f --all (everything) or -f --include a,b.
  -n, --dry-run          print the plan, change nothing (safe anywhere)
  -h, --help             show help (never executes anything)
`)
}

// parseTargetFlagsLenient is like parseTargetFlags but does NOT reject
// unknown --flags: it only extracts the known targeting flags and leaves
// everything else (including provider-binary flags like --force) in rest
// for pass-through. Used by delegation commands (summary/report/hot-restart,
// proxy refresh/remove-dead) where trailing args belong to the provider
// binary, not this tool.
func parseTargetFlagsLenient(args []string) (Target, []string, error) {
	return parseTargetFlagsInner(args, false)
}

// parseTargetFlags extracts targeting flags from args and returns the
// remaining positional args. Unknown -x flags are left in place (subcommands
// may define their own).
//
// Unknown --flags are REJECTED (review finding L2) — a typo like --netwrok
// or --dryrun must not be silently absorbed, because on a single-provider
// box the command would then proceed as a real action with no notice.
func parseTargetFlags(args []string) (Target, []string, error) {
	return parseTargetFlagsInner(args, true)
}

// parseTargetFlagsInner implements both variants. When strict is true,
// unknown --flags are rejected; otherwise they are preserved in rest.
func parseTargetFlagsInner(args []string, strict bool) (Target, []string, error) {
	var t Target
	var rest []string
	// Conflicting targeting flags are an error: matchProvider applies the
	// FIRST set field and silently ignores the rest, so `--unit x --user y`
	// would act on unit x while pretending to scope by user (free-review
	// major). Only one selector may be set; a same-field repeat just
	// overwrites.
	setField := func(flag, value string, field *string) error {
		if value == "" {
			return fmt.Errorf("%s requires a value", flag)
		}
		if *field != "" {
			// Same field, different value — last one wins (harmless).
			*field = value
			return nil
		}
		if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
			return fmt.Errorf("%s=%s conflicts with already-set target selector; specify exactly one of --unit/--user/--network/--network-id/--state-dir", flag, value)
		}
		*field = value
		return nil
	}
	// Pre-pass: accept both "--user value" and "--user=value". The equals
	// form is standard Unix convention and was historically rejected here,
	// which made the target flags unusable on multi-provider boxes for
	// subcommands that rejected unknown --flags (e.g. update builds its own
	// flag loop on the leftover args). Expand "=" into the space form so the
	// switch below handles both identically. Done in a dedicated expansion
	// pass over a copy to avoid mutating the slice while iterating it.
	expanded := make([]string, 0, len(args))
	for _, a := range args {
		matched := false
		for _, f := range []string{"--unit", "--user", "--network", "--network-id", "--state-dir"} {
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				if v == "" {
					return t, nil, fmt.Errorf("%s requires a value", f)
				}
				expanded = append(expanded, f, v)
				matched = true
				break
			}
		}
		if !matched {
			expanded = append(expanded, a)
		}
	}
	args = expanded
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--unit":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--unit requires a value")
			}
			if err := setField("--unit", args[i+1], &t.Unit); err != nil {
				return t, nil, err
			}
			i++
		case "--user":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--user requires a value")
			}
			if err := setField("--user", args[i+1], &t.User); err != nil {
				return t, nil, err
			}
			i++
		case "--network":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--network requires a value")
			}
			if err := setField("--network", args[i+1], &t.Network); err != nil {
				return t, nil, err
			}
			i++
		case "--network-id":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--network-id requires a value")
			}
			if err := setField("--network-id", args[i+1], &t.NetworkID); err != nil {
				return t, nil, err
			}
			i++
		case "--state-dir":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--state-dir requires a value")
			}
			if err := setField("--state-dir", args[i+1], &t.StateDir); err != nil {
				return t, nil, err
			}
			i++
		default:
			// Reject unknown flags instead of silently dropping them — a
			// typo like --netwrok or --dryrun would otherwise be absorbed
			// and the command proceeds as if un-targeted (review finding
			// L2; on a single-provider box that means a real action with
			// no dry-run notice).
			if strict && strings.HasPrefix(args[i], "--") {
				return t, nil, fmt.Errorf("unknown flag %q", args[i])
			}
			rest = append(rest, args[i])
		}
	}
	return t, rest, nil
}

// cmdProviders lists every provider on the box as a table. When no systemd
// providers exist but docker provider containers do, it says so and points
// at urnet-docker (which has its own providers listing).
func cmdProviders(args []string) error {
	providers := Discover()
	if len(providers) == 0 {
		docker := DiscoverDocker()
		if len(docker) == 0 {
			fmt.Println("no providers found on this box")
			return nil
		}
		fmt.Println("no systemd providers found on this box; running in docker (use urnet-docker):")
		for _, p := range docker {
			fmt.Printf("  %s  net=%s\n", p.Unit, p.Network)
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PID	USER	UNIT	NETWORK	NET-ID	STATE-DIR	BIN	VER")
	for _, p := range providers {
		pid := "-"
		if p.PID > 0 {
			pid = fmt.Sprintf("%d", p.PID)
		}
		ver := p.Version
		if ver == "" {
			ver = "-"
		}
		netID := shortID(p.NetworkID)
		fmt.Fprintf(w, "%s	%s	%s	%s	%s	%s	%s	%s\n",
			pid, p.User, p.Unit, p.Network, netID, p.StateDir, p.Binary, ver)
	}
	return w.Flush()
}

// shortID renders a UUID-ish id as its first 8 chars for table display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// cmdStatus shows detailed info for one provider (targeted).
func cmdStatus(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, narrowed, err := selectTargetOrSoleAccessible(providers, t)
	if err != nil {
		return err
	}
	if narrowed {
		printNarrowedNote(len(providers), p, "status")
	}
	// Windows and macOS get the styled status panel; Linux keeps its
	// systemd-oriented table untouched.
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		renderStatusPanel(p)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "user:\t%s\n", p.User)
	fmt.Fprintf(w, "unit:\t%s\n", p.Unit)
	fmt.Fprintf(w, "binary:\t%s\n", p.Binary)
	fmt.Fprintf(w, "version:\t%s\n", p.Version)
	fmt.Fprintf(w, "state-dir:\t%s\n", p.StateDir)
	fmt.Fprintf(w, "pid:\t%d\n", p.PID)
	fmt.Fprintf(w, "running:\t%v\n", p.Running)
	fmt.Fprintf(w, "network:\t%s\n", p.Network)
	fmt.Fprintf(w, "network-id:\t%s\n", p.NetworkID)
	exp := "n/a"
	if !p.JWTExpires.IsZero() {
		exp = p.JWTExpires.Format(time.RFC3339)
	}
	fmt.Fprintf(w, "jwt-expires:\t%s\n", exp)
	return w.Flush()
}

// renderStatusPanel prints a compact status summary with a header bar,
// a divider, and an optional proxies section. Used on Windows and macOS;
// Linux keeps its original table output.
func renderStatusPanel(p Provider) {
	state, badge := "STOPPED", "O"
	color := ""
	if p.Running {
		state, badge = "RUNNING", "@"
		color = "\x1b[32m"
	}

	exp := "n/a"
	if !p.JWTExpires.IsZero() {
		exp = p.JWTExpires.Format(time.RFC3339)
	}
	pidTxt := "-"
	if p.PID != 0 {
		pidTxt = strconv.Itoa(p.PID)
	}
	type row struct{ k, v string }
	rows := []row{
		{"user", orDash(p.User)},
		{"unit", orDash(p.Unit)},
		{"binary", orDash(p.Binary)},
		{"version", orDash(p.Version)},
		{"state dir", orDash(p.StateDir)},
		{"pid", pidTxt},
		{"network", orDash(p.Network)},
		{"network id", orDash(p.NetworkID)},
		{"jwt expires", exp},
	}

	keyW := 0
	for _, r := range rows {
		if len(r.k) > keyW {
			keyW = len(r.k)
		}
	}
	const maxValW = 60
	valW := 0
	for _, r := range rows {
		w := len(r.v)
		if w > maxValW {
			w = maxValW
		}
		if w > valW {
			valW = w
		}
	}
	if valW < 12 {
		valW = 12
	}

	// Header bar: title + status on the right.
	title := orDash(p.Network)
	if title == "-" {
		title = orDash(p.User)
	}
	if color != "" {
		state = color + state + "\x1b[0m"
	}
	fmt.Printf("PROVIDER STATUS   %s", title)
	fmt.Printf(" %s %s\n", badge, state)
	divW := keyW + 2 + valW
	if divW < 70 {
		divW = 70
	}
	fmt.Printf("  %s\n", strings.Repeat("-", divW))

	// Rows.
	for _, r := range rows {
		fmt.Printf("  %-*s %s\n", keyW+1, r.k+":", clamp(r.v, maxValW))
	}

	// Proxies section.
	printProxyStatus(p)
}

// printProxyStatus prints the provider's proxy summary: total/up from the
// proxy_health.state snapshot, and configured URL/file sources. It degrades
// to "n/a"/"none" if no proxy state or sources exist.
func printProxyStatus(p Provider) {
	fmt.Printf("  %s\n", strings.Repeat("-", 70))

	keyW := len("file sources:")
	// Proxy health state: proxy_health.state in the state dir.
	up, total, haveHealth := readProxyHealth(p.StateDir)
	if haveHealth {
		b := "DOWN"
		if up > 0 {
			b = "UP"
		}
		fmt.Printf("  %-*s %d up / %d total   [%s]\n", keyW+1, "PROXIES:", up, total, b)
	} else {
		fmt.Printf("  %-*s n/a  (no proxy health state)\n", keyW+1, "PROXIES:")
	}

	// URL sources from proxy_url.json (sources list).
	urlSrc := readProxyURLSources(p.StateDir)
	if len(urlSrc) > 0 {
		fmt.Printf("  %-*s %s\n", keyW+1, "URL sources:", strings.Join(urlSrc, ", "))
	} else {
		fmt.Printf("  %-*s none\n", keyW+1, "URL sources:")
	}

	// File source: the --proxy_file path from proxy.state / the provider's
	// config, if discoverable.
	fileSrc := readProxyFileSource(p)
	if fileSrc != "" {
		fmt.Printf("  %-*s %s\n", keyW+1, "file sources:", fileSrc)
	} else {
		fmt.Printf("  %-*s none\n", keyW+1, "file sources:")
	}
}

// clamp truncates s to at most max runes, appending "..." if truncated.
func clamp(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// readProxyHealth reads the proxy_health.state snapshot and returns
// (up, total, ok). A missing/unparseable file yields ok=false.
func readProxyHealth(stateDir string) (up, total int, ok bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, "proxy_health.state"))
	if err != nil {
		return 0, 0, false
	}
	// Best-effort parse: count lines that indicate a healthy vs total proxy.
	// The exact shape is provider-binary controlled; degrade gracefully.
	lines := strings.Split(string(b), "\n")
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue // skip trailing/blank lines so total is not inflated
		}
		total++
		low := strings.ToLower(ln)
		// A line counts as up if it contains a status token (up/ok/healthy)
		// on a word boundary, to avoid "upstream"/"oklahoma" false positives.
		if statusLineUp(low) {
			up++
		}
	}
	if total == 0 {
		return 0, 0, false
	}
	return up, total, true
}

// readProxyURLSources returns the configured URL proxy sources from
// proxy_url.json (the "sources" field).
// statusLineUp reports whether a lowercased proxy-health line indicates a
// live proxy, matching status tokens on word boundaries only.
var statusUpRe = regexp.MustCompile(`\b(?:up|ok|healthy)\b`)

// statusLineUp reports whether a lowercased proxy-health line indicates a
// live proxy, matching status tokens on whole-word boundaries only (so
// "upstream" or "oklahoma" are not treated as up/ok).
func statusLineUp(low string) bool {
	return statusUpRe.MatchString(low)
}

func readProxyURLSources(stateDir string) []string {
	b, err := os.ReadFile(filepath.Join(stateDir, "proxy_url.json"))
	if err != nil {
		return nil
	}
	var st struct {
		Sources []string `json:"sources"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil
	}
	if len(st.Sources) == 0 {
		return nil
	}
	return st.Sources
}

// readProxyFileSource returns the proxy file path if the provider uses one
// (from proxy.state "source"), else "".
func readProxyFileSource(p Provider) string {
	b, err := os.ReadFile(filepath.Join(p.StateDir, "proxy.state"))
	if err != nil {
		return ""
	}
	var st struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return ""
	}
	if st.Source == "" {
		return ""
	}
	return st.Source
}

// stdinReader is the ONE buffered reader over stdin, shared by every
// interactive prompt (confirm gates, update confirm, the provider picker).
// Each prompt MUST read from this single reader: a second bufio.Reader over
// the same fd would lose whatever the first already buffered, so piped
// input (`echo y | urnet-tools update --all`) hangs on the second prompt
// (free-review HIGH, mimo-v2.5).
var stdinReader = bufio.NewReader(os.Stdin)

// stdinIsInteractiveOverride, when non-nil, replaces the terminal check.
// Tests that feed stdin via a substituted reader (strings.Reader, pipe) set
// this to true so the shared-reader behavior is testable without a real TTY.
// Unsynchronized package global: tests that touch it MUST NOT call
// t.Parallel() (none do today — keep it that way).
var stdinIsInteractiveOverride func() bool

// stdinIsInteractive reports whether stdin is a terminal. Every confirm
// prompt MUST gate on this BEFORE reading: a ReadString on an open-but-silent
// pipe (cron, CI, MCP exec, a shell that left stdin open) blocks forever — it
// never sees EOF, so the err != nil path never fires. Non-interactive runs
// must use -f/--force (or --yes); refusing with a clear message beats hanging
// (gauntlet finding BUG-14: self-update blocked on read(0) for minutes).
// Uses term.IsTerminal (ioctl-based) rather than ModeCharDevice, which
// misclassifies /dev/zero and other char devices as terminals (CodeRabbit
// review finding).
func stdinIsInteractive() bool {
	if stdinIsInteractiveOverride != nil {
		return stdinIsInteractiveOverride()
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmStdinRead performs one confirmation line read, refusing cleanly on
// non-interactive stdin instead of blocking. Shared by every prompt.
func confirmStdinRead(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if !stdinIsInteractive() {
		return "", fmt.Errorf("stdin is not a terminal; use -f/--force to skip the prompt (or --yes where supported)")
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line, nil
}

// confirmGateMulti is the batch variant of confirmGate: it lists every
// provider in the chosen set before the yes/no prompt.
//
// The listing is printed UNCONDITIONALLY (to stderr, so it doesn't pollute
// piped stdout) — even with -f/--force, which only bypasses the interactive
// prompt. Scripted/cron runs are the primary -f users and the most likely to
// be replayed unattended; a printed "about to touch: X, Y" line in the log
// is the audit trail for the incident class (review finding M1).
func confirmGateMulti(op string, targets []Provider, force, dryRun bool) (bool, error) {
	fmt.Fprintf(os.Stderr, "[urnet-tools] %s:\n", op)
	for _, p := range targets {
		fmt.Fprintf(os.Stderr, "  %s (user=%s, network=%s, state=%s)\n", providerLabel(p), p.User, p.Network, p.StateDir)
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "[dry-run] no changes made\n")
		return false, nil // caller must not act
	}
	if force {
		return true, nil
	}
	line, err := confirmStdinRead("Type 'yes' to continue: ")
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return false, fmt.Errorf("aborted (confirmation did not match)")
	}
	return true, nil
}

// confirmGate implements the dry-run + confirm gate for destructive ops.
// With dryRun it prints the effect and returns a sentinel "skip" so callers
// can proceed without acting. With force it proceeds silently. Otherwise it
// prompts on the terminal and requires an explicit "yes".
//
// Like confirmGateMulti, the target is always printed (stderr) even under
// -f — the listing is the audit trail, only the prompt is gated.
func confirmGate(op string, target Provider, force, dryRun bool) (bool, error) {
	// Always print the target to stderr (audit trail), even under -f.
	fmt.Fprintf(os.Stderr, "[urnet-tools] %s: %s (user=%s, network=%s, state=%s)\n",
		op, providerLabel(target), target.User, target.Network, target.StateDir)
	if dryRun {
		fmt.Fprintf(os.Stderr, "[dry-run] no changes made\n")
		return false, nil // caller must not act
	}
	if force {
		return true, nil
	}
	line, err := confirmStdinRead("Type 'yes' to continue: ")
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return false, fmt.Errorf("aborted (confirmation did not match)")
	}
	return true, nil
}
