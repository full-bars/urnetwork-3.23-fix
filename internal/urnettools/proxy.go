package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// providerSubcommand runs a provider-binary subcommand against the targeted
// provider, streaming stdout/stderr. This is the delegation pattern the
// legacy shell tool uses ("$provider_bin proxy add --proxy_file=X -f") — the
// Go tool resolves WHICH provider, then hands the operation to that binary.
// Never runs against the wrong provider: target selection happens first.
func providerSubcommand(p Provider, args ...string) error {
	if p.Binary == "" {
		return fmt.Errorf("provider %s has no resolvable binary path", providerLabel(p))
	}
	cmd := exec.Command(p.Binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Run with the provider's HOME so state lands in the right directory.
	if p.User != "" {
		if home := homeForUser(p.User); home != "" {
			cmd.Env = append(os.Environ(), "HOME="+home)
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provider %s: %v", providerLabel(p), err)
	}
	return nil
}

// homeForUser returns the home directory for an OS user via getent.
func homeForUser(user string) string {
	out, err := exec.Command("getent", "passwd", user).Output()
	if err != nil {
		return ""
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) >= 6 {
		return fields[5]
	}
	return ""
}

// cmdProxy dispatches proxy sub-operations to the targeted provider(s).
// Usage: urnet-tools proxy add <file> | clear | remove | refresh [targets]
func cmdProxy(args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("proxy requires a subcommand: add <file> | clear | remove | refresh")
	}
	sub := args[0]
	rest := args[1:]
	// refresh passes provider-binary flags through (e.g. --force), so it
	// needs the LENIENT parse (unknown --flags preserved); every other
	// subcommand keeps strict rejection so a typo like --netwrok cannot be
	// silently absorbed on a destructive op (review finding L2).
	var t Target
	var err error
	if sub == "refresh" {
		t, rest, err = parseTargetFlagsLenient(rest)
	} else {
		t, rest, err = parseTargetFlags(rest)
	}
	if err != nil {
		return err
	}
	// Parse batch selection flags the same way update does.
	var include, exclude []string
	all := false
	interactive := forceInteractive(force)
	var positionals []string
	for _, a := range rest {
		switch {
		case a == "--all" || a == "-all":
			all = true
		case a == "--select":
			interactive = !force
		case strings.HasPrefix(a, "--include="):
			include = splitLabels(strings.TrimPrefix(a, "--include="))
		case strings.HasPrefix(a, "--exclude="):
			exclude = splitLabels(strings.TrimPrefix(a, "--exclude="))
		case a == "--include" || a == "--exclude":
			return fmt.Errorf("%s requires a value (use --%s=a,b)", a, strings.TrimPrefix(a, "--"))
		default:
			positionals = append(positionals, a)
		}
	}

	providers := Discover()
	var chosen []Provider
	if all {
		// --all conflicts with an explicit target — error rather than
		// silently discarding it (review finding M4).
		if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
			return fmt.Errorf("--all conflicts with an explicit target (%s); use one or the other", t)
		}
		if len(providers) == 0 {
			return fmt.Errorf("no providers found on this box")
		}
		chosen = providers
	} else {
		chosen, err = selectTargets(providers, t, include, exclude, interactive)
		if err != nil {
			return err
		}
	}

	// Build the provider-binary args for this sub-op.
	var opArgs []string
	switch sub {
	case "add":
		if len(positionals) < 1 {
			return fmt.Errorf("proxy add requires a proxy file path")
		}
		file := positionals[0]
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("proxy file not found: %s", file)
		}
		opArgs = []string{"proxy", "add", "--proxy_file=" + file, "-f"}
	case "clear":
		opArgs = []string{"proxy", "clear"}
	case "remove":
		opArgs = []string{"proxy", "remove", "--all"}
	case "refresh":
		opArgs = []string{"proxy", "refresh"}
		// Positional args after refresh are passed through (e.g. --force).
		opArgs = append(opArgs, positionals...)
	case "health":
		// State-file based; not a provider-binary delegation. Uses the
		// already-parsed target (t) — re-parsing here would see an arg
		// list with the target flags already stripped (free-review
		// major: health/traffic/remove-dead lost targeting).
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		return cmdProxyHealthTarget(p)
	case "traffic":
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		return cmdProxyTrafficTarget(p)
	case "remove-dead":
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		return providerSubcommand(p, append([]string{"proxy", "remove-dead"}, positionals...)...)
	default:
		return fmt.Errorf("unknown proxy subcommand %q (add|clear|health|traffic|refresh|remove-dead)", sub)
	}

	// Destructive gate for clear/remove; add/refresh are additive.
	if sub == "clear" || sub == "remove" {
		ok, err := confirmGateMulti(fmt.Sprintf("%s proxies on %d provider(s)", sub, len(chosen)), chosen, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil // dry-run
		}
	} else if dryRun {
		fmt.Printf("[dry-run] would %s on %d provider(s)\n", strings.Join(opArgs, " "), len(chosen))
		for _, p := range chosen {
			fmt.Printf("  %s (user=%s, network=%s)\n", providerLabel(p), p.User, p.Network)
		}
		return nil
	}

	for _, p := range chosen {
		if err := providerSubcommand(p, opArgs...); err != nil {
			return err
		}
	}
	return nil
}
