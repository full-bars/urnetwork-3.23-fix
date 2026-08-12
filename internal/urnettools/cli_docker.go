package urnettools

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
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

// RunDocker is the CLI entry point for the urnet-docker binary. It mirrors
// urnet-tools' targeting/confirm-gate philosophy but discovers docker
// containers and delegates provider internals via docker exec.
func RunDocker(args []string) error {
	if len(args) == 0 {
		usageDocker()
		return nil
	}
	op := args[0]
	rest := args[1:]
	switch op {
	case "providers", "list", "ps":
		if hasHelpFlag(rest) {
			usageDocker()
			return nil
		}
		return cmdDockerProviders(rest)
	case "status":
		if hasHelpFlag(rest) {
			usageDocker()
			return nil
		}
		return cmdDockerStatus(rest)
	case "exec":
		// NOTE: no broad hasHelpFlag here — an inner `--help` after the `--`
		// separator belongs to the container command, not urnet-docker
		// (coderabbit major). splitExecArgs handles -h/--help on the
		// PRE-separator (urnet-docker) side only.
		return cmdDockerExec(rest)
	case "restart":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdDockerRestart(rest2, force, dryRun)
	case "update", "self-update", "selfupdate":
		// Docker containers update by pulling new images (out of this
		// tool's scope); `urnet-docker update` refreshes the TOOL binary
		// itself. Host-side only — never executed inside a container.
		// Help must show the DOCKER usage (the shared parseGlobalFlags
		// would print the urnet-tools one — verified 2026-08-12 review).
		if hasHelpFlag(rest) {
			usageDocker()
			return nil
		}
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdSelfUpdate(rest2, force, dryRun)
	case "logs":
		if hasHelpFlag(rest) {
			usageDocker()
			return nil
		}
		return cmdDockerLogs(rest)
	case "help", "-h", "--help":
		usageDocker()
		return nil
	default:
		return fmt.Errorf("unknown command %q (see 'urnet-docker help')", op)
	}
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

Commands:
  providers              list all provider containers (identified by in-container JWT)
  status [target]        detailed status of one container
  exec [target flags] [--] <cmd...> run a command inside the container; target flags
                          (--unit/--network/etc) must precede the command; use "--" to
                          forward inner flags verbatim, e.g.
                          urnet-docker exec --unit <name> -- urnet-tools proxy add --proxy_file=/tmp/p.txt
  exec <cmd...>           command-first form still works (target flags optional;
                          required when more than one provider container exists)
  restart [target]       restart the container
  update                 update this tool binary itself (containers update by image pull)
  logs [target] [N]    follow the container's logs (last N lines, default 250;
                          RAMLOGS-aware: streams /dev/shm when enabled, else docker logs;
                          interactive picker when multiple providers)

Targeting flags (required when more than one provider container exists):
  --unit <name>          container name (mapped to Unit)
  --network <name>       JWT network name, e.g. tacogonzalez3000
  --state-dir <path>     state dir INSIDE the container (rarely needed)

Global flags:
  -f, --force            bypass the confirm gate (for scripts/cron)
  -n, --dry-run          show what would happen without doing it
  -h, --help             show help (never executes anything)
`)
}

// dockerTargetFromArgs reuses parseTargetFlags; container targets map the
// --unit flag to the container name.
func dockerTargetFromArgs(args []string) (Target, []string, error) {
	return parseTargetFlags(args)
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
	t, _, err := dockerTargetFromArgs(args)
	if err != nil {
		return err
	}
	providers := DiscoverDocker()
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

// cmdDockerRestart restarts a container (destructive gate applies).
func cmdDockerRestart(args []string, force, dryRun bool) error {
	t, _, err := dockerTargetFromArgs(args)
	if err != nil {
		return err
	}
	providers := DiscoverDocker()
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
	t, rest, err := dockerTargetFromArgs(args)
	if err != nil {
		return err
	}
	n, err := parseLogLineCount(rest)
	if err != nil {
		return err
	}
	providers := DiscoverDocker()
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
