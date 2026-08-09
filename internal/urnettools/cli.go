package urnettools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// Run is the CLI entry point. It parses subcommands and dispatches. The
// legacy shell dispatcher bug — `--help` executing a destructive op — cannot
// exist here because every subcommand parses its own flags before acting.
func Run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	op := args[0]
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
	case "proxy":
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
		if herr != nil {
			return nil // help printed, never executes
		}
		return cmdSimpleDelegation("summary", rest2)
	case "report":
		rest2, herr := parseDelegationArgs(rest)
		if herr != nil {
			return nil // help printed, never executes
		}
		return cmdSimpleDelegation("report", rest2)
	case "hot-restart", "hotrestart":
		rest2, herr := parseDelegationArgs(rest)
		if herr != nil {
			return nil // help printed, never executes
		}
		return cmdSimpleDelegation("hot-restart", rest2)
	case "start":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = force, dryRun
		return cmdStart(rest2)
	case "stop":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = force, dryRun
		return cmdStop(rest2)
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

// cmdSimpleDelegation handles the pass-through commands (summary, report,
// hot-restart): resolve the targeted provider, then delegate the exact
// subcommand to that provider's binary.
func cmdSimpleDelegation(sub string, args []string) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	// Delegate: <binary> <sub> <rest...>
	cmdArgs := append([]string{sub}, rest...)
	return providerSubcommand(p, cmdArgs...)
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

Commands:
  providers              list all providers on this box (all users, JWT identity)
  status [target]        detailed status of one provider
  update [target]        update provider(s) to latest (interactive; --tag to pin)
  proxy add|clear|remove|refresh [target]   manage proxies
  summary [target]       fleet-style summary for one provider
  report <url> [target]  set hub URL at runtime
  hot-restart [target]   restart one provider's unit

Providers are identified three ways (use any):
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

// cmdProviders lists every provider on the box as a table.
func cmdProviders(args []string) error {
	providers := Discover()
	if len(providers) == 0 {
		fmt.Println("no providers found on this box")
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
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
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

// stdinReader is the ONE buffered reader over stdin, shared by every
// interactive prompt (confirm gates, update confirm, the provider picker).
// Each prompt MUST read from this single reader: a second bufio.Reader over
// the same fd would lose whatever the first already buffered, so piped
// input (`echo y | urnet-tools update --all`) hangs on the second prompt
// (free-review HIGH, mimo-v2.5).
var stdinReader = bufio.NewReader(os.Stdin)

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
	fmt.Fprint(os.Stderr, "Type 'yes' to continue: ")
	line, err := stdinReader.ReadString('\n')
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
	fmt.Fprint(os.Stderr, "Type 'yes' to continue: ")
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return false, fmt.Errorf("aborted (confirmation did not match)")
	}
	return true, nil
}
