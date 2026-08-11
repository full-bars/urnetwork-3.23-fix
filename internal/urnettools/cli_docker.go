package urnettools

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

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
		if hasHelpFlag(rest) {
			usageDocker()
			return nil
		}
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
  exec <cmd...> [target] run a command inside the container; use "--" before
                          the command to pass inner flags verbatim, e.g.
                          urnet-docker exec --unit <name> -- urnet-tools proxy add --proxy_file=/tmp/p.txt
  restart [target]       restart the container
  logs [target]          follow the container's logs (RAMLOGS-aware)

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
	p, err := selectTarget(providers, t)
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
	// the container command (docker/git/ssh convention) so inner-command
	// flags like -f or --verbose can never be mistaken for urnet-docker
	// flags or silently dropped.
	pre, rest, err := splitExecArgs(args)
	if err == errHelpShown {
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
		return fmt.Errorf("exec requires a command, e.g. 'urnet-docker exec --unit urnet -- urnet-tools proxy add --proxy_file=/tmp/p.txt'")
	}
	providers := DiscoverDocker()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	// p.Unit holds the container name; delegate the command verbatim.
	return containerExecByName(p.Unit, rest...)
}

// splitExecArgs divides exec arguments into the pre-command urnet-docker
// targeting flags and the verbatim in-container command. A `--` separator
// puts EVERYTHING after it into the command (docker/git/ssh convention);
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
			split += 2 // flag + value
		case "-h", "--help":
			// Belt-and-suspenders: RunDocker already handles -h/--help via
			// hasHelpFlag before dispatching, but keep it here so a direct
			// call never misroutes help into a delegated action.
			return nil, nil, errHelpShown
		default:
			// Unknown leading flag: refuse rather than silently drop it
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

// cmdDockerLogs tails logs for the targeted container. When the container
// runs with URNETWORK_RAMLOGS, this reads /dev/shm inside the container via
// docker exec; otherwise it uses docker logs.
func cmdDockerLogs(args []string) error {
	t, rest, err := dockerTargetFromArgs(args)
	if err != nil {
		return err
	}
	n := "200"
	if len(rest) > 0 {
		n = rest[0]
	}
	providers := DiscoverDocker()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	// Prefer RAMLOGS file if present; fall back to docker logs.
	out, err := containerReadFileSafe(p.Unit, "/dev/shm/urnetwork.log")
	if err == nil && len(out) > 0 {
		fmt.Print(tailLines(out, n))
		return nil
	}
	return containerLogs(p.Unit, n)
}
