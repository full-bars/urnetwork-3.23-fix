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
	// -h/--help anywhere in the proxy args shows proxy help and returns
	// without executing (gauntlet finding BUG-2 + Sonnet review: help can
	// appear at any position, e.g. `proxy add <file> --help` or
	// `proxy refresh --force -h` — the latter previously reached the
	// interactive picker and blocked on EOF).
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintf(os.Stderr, `urnet-tools proxy — manage a provider's proxies

Usage: urnet-tools proxy <subcommand> [target] [flags]

Subcommands:
  add <file>             add proxies from a proxy file (host:port[:user:pass])
  clear                  remove all proxies
  remove                 remove proxies (--all, or target-specific)
  refresh                re-read the proxy source (--force to force)
  add-source <url>       add a URL proxy source (fetched + cached)
  remove-source <url>    remove a URL proxy source
  health                 proxy health from state files (single target)
  traffic                proxy traffic from state files (single target)
  remove-dead            remove dead/degraded proxies (single target)

Targets and batch flags work as for other commands (--unit/--user/--network,
--all/--include/--exclude/--select). See 'urnet-tools help' for targeting.
`)
			return nil
		}
	}
	// LENIENT target parse for ALL subcommands: proxy defines its own
	// batch flags (--all/--select/--include/--exclude) consumed below, and
	// refresh/remove-dead additionally pass provider-binary flags through
	// (e.g. --force). Strict parsing here rejected those as unknown before
	// the loop ran (opus5 F1: `proxy clear --all` was dead). Leftover
	// unknown --flags are rejected after the loop, except for the
	// pass-through subcommands.
	t, rest, err := parseTargetFlagsLenient(rest)
	if err != nil {
		return err
	}
	// Parse batch selection flags the same way update does.
	var include, exclude []string
	all := false
	interactive := forceInteractive(force)
	var positionals []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
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
			// Also accept the space-separated form (--include a,b), matching
			// update's syntax (opus5 F1 note: the two commands disagreed).
			if i+1 < len(rest) {
				if a == "--include" {
					include = splitLabels(rest[i+1])
				} else {
					exclude = splitLabels(rest[i+1])
				}
				i++
				break
			}
			return fmt.Errorf("%s requires a value (use --%s=a,b)", a, strings.TrimPrefix(a, "--"))
		default:
			// Unknown --flag: pass through only for refresh/remove-dead
			// (provider-binary flags like --force); reject elsewhere so a
			// typo like --netwrok cannot be silently absorbed on a
			// destructive op (review finding L2; opus5 F1).
			if strings.HasPrefix(a, "-") && sub != "refresh" && sub != "remove-dead" {
				return fmt.Errorf("unknown flag %q for proxy %s", a, sub)
			}
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
		// The dispatcher's parseGlobalFlags consumes -f/--force as the
		// GLOBAL force flag before cmdProxy runs, so a `refresh --force`
		// invocation never forwards --force to the provider binary — the
		// provider's warmup gate then refuses (exit 52) even though the
		// operator asked to force (gauntlet finding BUG-9). Re-add it
		// when the global force flag was set.
		if force {
			opArgs = append(opArgs, "--force")
		}
		// Positional args after refresh are passed through (e.g. --force).
		opArgs = append(opArgs, positionals...)
	case "add-source", "remove-source":
		// URL-source management (gauntlet finding BUG-8): the provider
		// binary implements `provider proxy add-source <url>` /
		// `remove-source <url>`, but the Go tool previously had no such
		// subcommand, so URL proxies (a core fleet feature) were
		// unmanageable through the new tool. Single-target (a URL source
		// is per-provider), like health/traffic.
		if all || len(include) > 0 || len(exclude) > 0 || interactive != forceInteractive(force) {
			return fmt.Errorf("proxy %s operates on ONE provider — --all/--include/--exclude/--select do not apply; use --unit/--user/--network to target it", sub)
		}
		if len(positionals) < 1 {
			return fmt.Errorf("proxy %s requires a URL", sub)
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		return providerSubcommand(p, append([]string{"proxy", sub}, positionals...)...)
	case "health", "traffic", "remove-dead":
		// These are single-target subcommands (selectTarget, not
		// selectTargets) — batch flags are meaningless here and must not be
		// silently dropped (free-review MEDIUM).
		if all || len(include) > 0 || len(exclude) > 0 || interactive != forceInteractive(force) {
			return fmt.Errorf("proxy %s operates on ONE provider — --all/--include/--exclude/--select do not apply; use --unit/--user/--network to target it", sub)
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		switch sub {
		case "health":
			// State-file based; not a provider-binary delegation. Uses the
			// already-parsed target (t) — re-parsing here would see an arg
			// list with the target flags already stripped (free-review
			// major: health/traffic/remove-dead lost targeting).
			return cmdProxyHealthTarget(p)
		case "traffic":
			return cmdProxyTrafficTarget(p)
		default: // remove-dead
			return providerSubcommand(p, append([]string{"proxy", "remove-dead"}, positionals...)...)
		}
	default:
		return fmt.Errorf("unknown proxy subcommand %q (add|clear|health|traffic|refresh|remove-dead|add-source|remove-source)", sub)
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
