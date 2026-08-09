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
		return cmdProviders(rest)
	case "status":
		return cmdStatus(rest)
	case "update":
		force, dryRun, rest2, err := parseGlobalFlags(rest)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdUpdate(rest2, force, dryRun)
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

// errHelpShown is a sentinel: help was printed, not an error condition.
var errHelpShown = fmt.Errorf("help shown")

// usage prints the subcommand summary.
func usage() {
	fmt.Fprintf(os.Stderr, `urnet-tools — provider-aware URnetwork manager

Usage: urnet-tools <command> [flags]

Commands:
  providers              list all providers on this box (all users)
  status [target]        detailed status of one provider
  update [target]        update one provider's binary (stages on real disk)
  proxy add|clear|remove|refresh [target]   manage proxies
  summary [target]       fleet-style summary for one provider
  report <url> [target]  set hub URL at runtime
  hot-restart [target]   restart one provider's unit

Targeting flags (required when the box runs more than one provider):
  --unit <name>          systemd unit, e.g. urnetwork-native.service
  --user <user>          OS user, e.g. urnet
  --network <name>       JWT network name, e.g. tacogonzalez3000
  --state-dir <path>     explicit state directory

Global flags:
  -f, --force            bypass the confirm gate (for scripts/cron)
  -n, --dry-run          show what would happen without doing it
  -h, --help             show help (never executes anything)
`)
}

// parseTargetFlags extracts targeting flags from args and returns the
// remaining positional args. Unknown -x flags are left in place (subcommands
// may define their own).
func parseTargetFlags(args []string) (Target, []string, error) {
	var t Target
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--unit":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--unit requires a value")
			}
			t.Unit = args[i+1]
			i++
		case "--user":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--user requires a value")
			}
			t.User = args[i+1]
			i++
		case "--network":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--network requires a value")
			}
			t.Network = args[i+1]
			i++
		case "--state-dir":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("--state-dir requires a value")
			}
			t.StateDir = args[i+1]
			i++
		default:
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
	fmt.Fprintln(w, "PID\tUSER\tUNIT\tNETWORK\tSTATE-DIR\tBIN\tVER")
	for _, p := range providers {
		pid := "-"
		if p.PID > 0 {
			pid = fmt.Sprintf("%d", p.PID)
		}
		ver := p.Version
		if ver == "" {
			ver = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pid, p.User, p.Unit, p.Network, p.StateDir, p.Binary, ver)
	}
	return w.Flush()
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

// confirmGate implements the dry-run + confirm gate for destructive ops.
// With dryRun it prints the effect and returns a sentinel "skip" so callers
// can proceed without acting. With force it proceeds silently. Otherwise it
// prompts on the terminal and requires an explicit "yes".
func confirmGate(op string, target Provider, force, dryRun bool) (bool, error) {
	if dryRun {
		fmt.Printf("[dry-run] would %s: %s\n", op, providerLabel(target))
		return false, nil // caller must not act
	}
	if force {
		return true, nil
	}
	fmt.Printf("This will %s:\n  %s (user=%s, network=%s, state=%s)\n",
		op, providerLabel(target), target.User, target.Network, target.StateDir)
	fmt.Print("Type 'yes' to continue: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "yes" {
		return false, fmt.Errorf("aborted (confirmation did not match)")
	}
	return true, nil
}
