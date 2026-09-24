package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// containsAny reports whether args contains any of the given tokens.
func containsAny(args []string, tokens ...string) bool {
	for _, a := range args {
		for _, tok := range tokens {
			if a == tok {
				return true
			}
		}
	}
	return false
}

// providerSubcommand runs a provider-binary subcommand against the targeted
// provider, streaming stdout/stderr. This is the delegation pattern the
// legacy shell tool uses ("$provider_bin proxy add --proxy_file=X -f") — the
// Go tool resolves WHICH provider, then hands the operation to that binary.
// Never runs against the wrong provider: target selection happens first.
func providerSubcommand(p Provider, args ...string) error {
	if p.Binary == "" {
		return fmt.Errorf("provider %s has no resolvable binary path", providerLabel(p))
	}
	// The provider binary may have been deleted on disk by a prior update
	// while the process kept running on the freed inode; in that state a
	// plain exec fails with ENOENT. Restore the live image so every delegated
	// command still works against the ACTIVE provider, no restart needed.
	bin, recovered, err := ensureBinaryRecoverable(p)
	if err != nil {
		return err
	}
	if recovered {
		fmt.Fprintf(os.Stderr, "note: recovered deleted-but-running provider binary (%s, from /proc/%d/exe)\n", p.Binary, p.PID)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Wire stdin through for interactive subcommands. `proxy paste` reads its
	// proxy list from stdin and must get it even when piped (no TTY). The
	// interactive confirm subcommands (`remove-dead`, `remove`, `trim`, ...)
	// read the terminal through confirm(): without this the child inherits
	// /dev/null, confirm() hits EOF, and the user's "y" is never read —
	// "Remove N dead proxies? [y/N] Nothing to remove." with the answer
	// falling through to the shell. Gate those on stdinIsInteractive so
	// non-interactive (cron/systemd/pipe) invocations still see EOF and
	// refuse by default.
	// The paste dispatch builds args as ["proxy", "paste", ...], so args[1]
	// is the subcommand name here (args[0] is always "proxy").
	if len(args) > 0 && args[0] == "paste" || len(args) > 1 && args[0] == "proxy" && args[1] == "paste" {
		cmd.Stdin = os.Stdin
	} else if stdinIsInteractive() {
		cmd.Stdin = os.Stdin
	}
	// Run with the provider's HOME so state lands in the right directory.
	// When homeForUser fails, derive from the state dir's parent.
	home := homeForUser(p.User)
	if home == "" && p.StateDir != "" {
		home = filepath.Dir(p.StateDir)
	}
	if home != "" {
		cmd.Env = append(os.Environ(), "HOME="+home)
	} else {
		cmd.Env = os.Environ()
	}
	// Also run as that user when we are root, so auth/network files are written
	// owned by the provider user and remain readable by it. A failed drop is
	// an error, never a silent root fallback (see dropPrivilegesTo).
	if err := dropPrivilegesTo(p.User, cmd); err != nil {
		return fmt.Errorf("provider %s: %v", providerLabel(p), err)
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

// checkReadableAsUser verifies that the named user can open the file for
// reading. proxy add delegates to the provider binary via dropPrivilegesTo,
// so a file readable only by root or another user would fail inside the
// provider with a confusing permission error — check first. We compare the
// file's mode/owner to the user's uid/gid directly rather than spawning
// su/sudo: that resolves to a clear message and avoids needing the
// provider to be root for an explicit access(2) syscall. When we are not
// root, the caller's uid already determines read access — the caller is
// about to exec the provider as itself, so the stat is sufficient and we
// trust the existing access check.
func checkReadableAsUser(path, user string) error {
	if user == "" {
		return nil // nothing to check against; the caller will inherit
	}
	if os.Geteuid() != 0 {
		// Not root: the caller IS the provider user, the existing stat
		// (which succeeded) is the access check.
		return nil
	}
	uid, gid, err := lookupUserIDs(user)
	if err != nil {
		return fmt.Errorf("resolve uid/gid for %s: %w", user, err)
	}
	// M8 fix: walk the parent chain checking execute (search) permission
	// for the target uid. The old code only checked the file's own mode
	// bits, which passes for world-readable files inside non-traversable
	// directories (e.g. 0644 file inside 0700 /root).
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	for {
		fi, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat %s: %w", dir, err)
		}
		sysUID, sysGID := statFileOwnership(fi)
		perm := fi.Mode().Perm()
		// Check if user can traverse this directory
		canTraverse := false
		if perm&0o1 != 0 {
			canTraverse = true // world-execute
		} else if sysGID == int64(gid) && perm&0o10 != 0 {
			canTraverse = true // group-execute
		} else if sysUID == int64(uid) && perm&0o100 != 0 {
			canTraverse = true // owner-execute
		}
		if !canTraverse {
			return fmt.Errorf("proxy file %s is not reachable by user %s: no execute permission on %s", path, user, dir)
		}
		if dir == "/" || dir == "." {
			break
		}
		dir = filepath.Dir(dir)
	}
	// Now check the file itself
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("stat %s: %w", abs, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("proxy file %s is a symlink (refusing to follow)", abs)
	}
	st := fi.Mode().Perm()
	sysUID, sysGID := statFileOwnership(fi)
	// World-readable covers it
	if st&0o4 != 0 {
		return nil
	}
	// Group match
	if sysGID == int64(gid) && st&0o40 != 0 {
		return nil
	}
	// Owner match
	if sysUID == int64(uid) && st&0o400 != 0 {
		return nil
	}
	return fmt.Errorf("proxy file %s is not readable by user %s (owner=%v mode=%s)", abs, user, sysUID, fi.Mode().Perm())
}

// cmdProxy dispatches proxy sub-operations to the targeted provider(s).
// Usage: urnet-tools proxy add <file> | clear | remove | refresh [targets]
// withProxyRemoveYes re-adds the provider's own --yes to a `proxy remove`
// argv when the global force flag consumed it. The provider grammar accepts
// --yes only on the --match form (`proxy remove --match=<pattern> [--yes]`);
// address and --all removals reject it as a usage error, so it is appended
// only when a --match operand is present.
func withProxyRemoveYes(opArgs []string, force bool) []string {
	if !force || containsAny(opArgs, "--yes", "-y") {
		return opArgs
	}
	for _, a := range opArgs {
		if a == "--match" || strings.HasPrefix(a, "--match=") {
			return append(opArgs, "--yes")
		}
	}
	return opArgs
}

func cmdProxy(args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("proxy requires a subcommand: add <file> | paste | clear | remove | refresh | add-source <url> | remove-source <url> | health | traffic | ids | remove-dead | trim <N> | audit")
	}
	sub := args[0]
	rest := args[1:]
	// -h/--help anywhere in the proxy args shows proxy help and returns
	// without executing (help can
	// appear at any position, e.g. `proxy add <file> --help` or
	// `proxy refresh --force -h` — the latter previously reached the
	// interactive picker and blocked on EOF).
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintf(os.Stderr, `urnet-tools proxy — manage a provider's proxies

Usage: urnet-tools proxy <subcommand> [target] [flags]

Subcommands:
  add <file>             add proxies from a proxy file (host:port[:user:pass])
  paste                  paste proxies + source URLs, auto-normalize formats
  clear                  remove ALL proxies (unconditional)
  remove                 remove proxies: addresses or --match= given -> those
                         only; nothing given -> ALL proxies on the target
  refresh                re-read the proxy source (--force to force)
  add-source <url>       add a URL proxy source (fetched + cached)
  remove-source <url>    remove a URL proxy source
  health                 proxy health from state files (single target)
  traffic                proxy traffic from state files (single target)
  ids                    client_id per proxy from JWT store (single target)
  remove-dead            remove dead/degraded proxies (single target)
  trim <N>               hold running proxies at N, shed the A-F-worst (single target)
  audit [action]         inspect or toggle automated proxy audit engine (status|on|off|release)
  exclude                targeting flag, not a subcommand (see 'urnet-tools help')

Examples (proxy add):
  urnet-tools proxy add ~/proxies.txt                 # Linux / macOS
  urnet-tools proxy add C:\Users\<you>\proxies.txt     # Windows (\ or / separators)
  urnet-tools proxy audit status                      # Show proxy quality audit status
  urnet-tools proxy audit on                          # Enable automated quality enforcement
  urnet-tools proxy audit release 1.2.3.4:1080        # Unbench and release a parked proxy

Targets and batch flags work as for other commands (--unit/--user/--network,
--all/--include/--exclude). See 'urnet-tools help' for targeting.
`)
			return nil
		}
	}
	// LENIENT target parse for ALL subcommands: proxy defines its own
	// batch flags (--all/--include/--exclude) consumed below, and
	// refresh/remove-dead additionally pass provider-binary flags through
	// (e.g. --force). Strict parsing here rejected those as unknown before
	// the loop ran. Leftover
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
			if sub == "audit" && len(positionals) > 0 && positionals[0] == "release" {
				positionals = append(positionals, "--all")
			} else {
				all = true
			}
		case strings.HasPrefix(a, "--include="):
			include = splitLabels(strings.TrimPrefix(a, "--include="))
		case strings.HasPrefix(a, "--exclude="):
			exclude = splitLabels(strings.TrimPrefix(a, "--exclude="))
		case a == "--include" || a == "--exclude":
			// Also accept the space-separated form (--include a,b), matching
			// update's syntax.
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
		case strings.HasPrefix(a, "--file=") || strings.HasPrefix(a, "--proxy_file="):
			val := strings.TrimPrefix(a, "--file=")
			if strings.HasPrefix(a, "--proxy_file=") {
				val = strings.TrimPrefix(a, "--proxy_file=")
			}
			positionals = append(positionals, val)
		case a == "--file" || a == "--proxy_file":
			if i+1 < len(rest) {
				positionals = append(positionals, rest[i+1])
				i++
				break
			}
			return fmt.Errorf("%s requires a file path", a)
		case strings.HasPrefix(a, "--url=") || strings.HasPrefix(a, "--URL="):
			val := strings.TrimPrefix(a, "--url=")
			if strings.HasPrefix(a, "--URL=") {
				val = strings.TrimPrefix(a, "--URL=")
			}
			positionals = append(positionals, val)
		case a == "--url" || a == "--URL":
			if i+1 < len(rest) {
				positionals = append(positionals, rest[i+1])
				i++
				break
			}
			return fmt.Errorf("%s requires a URL", a)
		default:
			// Unknown --flag: pass through only for refresh/remove-dead
			// (provider-binary flags like --force); reject elsewhere so a
			// typo like --netwrok cannot be silently absorbed on a
			// destructive op.
			if strings.HasPrefix(a, "-") && sub != "refresh" && sub != "remove-dead" && sub != "remove" && sub != "paste" {
				return fmt.Errorf("unknown flag %q for proxy %s", a, sub)
			}
			positionals = append(positionals, a)
		}
	}

	providers := Discover()
	var chosen []Provider
	// Single-target subcommands (add with URL, add-source, remove-source,
	// trim, paste, health, traffic, ids, remove-dead) resolve their own
	// provider inside the dispatch (selectTarget). Pre-selecting here would
	// pop the interactive multi-select picker on a TTY and then THROW THE
	// PICK AWAY — the dispatch's selectTarget would refuse after the user
	// already chose. Only batch subcommands (add <file>, clear,
	// remove, refresh) act on a chosen SET.
	//
	// add <http://url> is single-target (resolves its own provider), so it
	// must not enter batch mode even though sub == "add" — only file-form
	// add is batch.
	batchSub := false
	if sub == "clear" || sub == "remove" || sub == "refresh" {
		batchSub = true
	} else if sub == "add" {
		// add <file> is batch; add <http://url> is single-target. The two
		// forms select providers differently, so one invocation cannot mix
		// them: the file form would run against the empty batch selection
		// and be skipped while the command still reported success.
		var haveURL, haveFile bool
		for _, target := range positionals {
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				haveURL = true
			} else {
				haveFile = true
			}
		}
		if haveURL && haveFile {
			return fmt.Errorf("proxy add cannot mix a proxy file and a URL in one command; run them separately")
		}
		batchSub = !haveURL
	}
	if batchSub {
		if all {
			// --all conflicts with an explicit target — error rather than
			// silently discarding it.
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
	}

	// Build the provider-binary args for this sub-op.
	var opArgs []string
	switch sub {
	case "add":
		if len(positionals) < 1 {
			return fmt.Errorf("proxy add requires a proxy file path or URL")
		}
		for _, target := range positionals {
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				if all || len(include) > 0 || len(exclude) > 0 {
					return fmt.Errorf("proxy add with URL operates on ONE provider — --all/--include/--exclude do not apply; use --unit/--user/--network to target it")
				}
				p, err := selectTarget(providers, t)
				if err != nil {
					return err
				}
				if dryRun {
					fmt.Printf("[dry-run] would add-source %s on %s\n", target, providerLabel(p))
					continue
				}
				if err := providerSubcommand(p, "proxy", "add-source", target); err != nil {
					return err
				}
				continue
			}
			file := expandHomePath(target)
			// Stat the file as the tool's user (existence check), then verify
			// the targeted provider user can actually read it: `proxy add` is
			// delegated to providerSubcommand → dropPrivilegesTo(p.User, cmd),
			// so the path must be accessible under the provider's uid/gid.
			// Checking only as the caller is racy (TOCTOU) AND lets root pass
			// files (e.g. /root/proxies.txt) that the provider user cannot open.
			if _, err := os.Stat(file); err != nil {
				return fmt.Errorf("proxy file not found: %s", file)
			}
			if len(chosen) > 0 {
				// Validate readability for every provider's user in the batch.
				// A shared file readable by all targeted users vs a file readable
				// only by the first provider's user — the delegated binary would
				// fail for every other user with a confusing error, so fail fast.
				for _, p := range chosen {
					if err := checkReadableAsUser(file, p.User); err != nil {
						return fmt.Errorf("provider %s: %w", providerLabel(p), err)
					}
				}
			}
			if dryRun {
				fmt.Printf("[dry-run] would proxy add --proxy_file=%s -f on %d provider(s)\n", file, len(chosen))
				for _, p := range chosen {
					fmt.Printf("  %s (user=%s, network=%s)\n", providerLabel(p), p.User, p.netLabel())
				}
				continue
			}
			for _, p := range chosen {
				if err := providerSubcommand(p, "proxy", "add", "--proxy_file="+file, "-f"); err != nil {
					return fmt.Errorf("provider %s: %w", providerLabel(p), err)
				}
			}
		}
		return nil
	case "clear":
		// The provider binary has no `proxy clear` subcommand (verified):
		// its docopt only has add/remove/remove-dead/refresh/add-source/
		// remove-source/exclude/summary. Map to remove --all, which clears
		// unconditionally (no docopt-valid force flag on this pattern).
		opArgs = []string{"proxy", "remove", "--all"}
	case "remove":
		// The provider binary supports per-address and per-pattern removal:
		//   provider proxy remove [<key_address>...] [--all]
		//   provider proxy remove --match=<pattern> [--yes] [--preview]
		// Pass through any addresses / --match given; ONLY when nothing is
		// specified do we default to --all. This fixes the old behavior
		// that unconditionally sent --all (silently wiping every proxy
		// regardless of what the user named).
		if len(positionals) > 0 {
			opArgs = append([]string{"proxy", "remove"}, positionals...)
		} else {
			opArgs = []string{"proxy", "remove", "--all"}
		}
		// H6: the dispatcher's parseGlobalFlags consumes -y/--yes as the
		// GLOBAL force flag before cmdProxy runs, so `proxy remove
		// --match=X --yes` never forwards the provider's own --yes — the
		// provider then prompts, and with stdin not passed through it reads
		// EOF, prints "Aborted." and exits 0 (a silent no-op reported as
		// success). Re-add --yes when the global force flag was set, exactly
		// like refresh re-adds --force.
		opArgs = withProxyRemoveYes(opArgs, force)
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
		if all || len(include) > 0 || len(exclude) > 0 {
			return fmt.Errorf("proxy %s operates on ONE provider — --all/--include/--exclude do not apply; use --unit/--user/--network to target it", sub)
		}
		if len(positionals) < 1 {
			return fmt.Errorf("proxy %s requires a URL", sub)
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		if dryRun {
			// remove-source is destructive; honor --dry-run here (it returns
			// before the shared gate below). add-source is additive but print
			// the plan uniformly.
			fmt.Printf("[dry-run] would %s %s on %s\n", sub, positionals[0], providerLabel(p))
			return nil
		}
		return providerSubcommand(p, append([]string{"proxy", sub}, positionals...)...)
	case "trim":
		// Single-target, destructive: shed the A-F-worst proxies down to N.
		if all || len(include) > 0 || len(exclude) > 0 {
			return fmt.Errorf("proxy trim operates on ONE provider — --all/--include/--exclude do not apply; use --unit/--user/--network to target it")
		}
		if len(positionals) < 1 {
			return fmt.Errorf("proxy trim requires a target count, e.g. 'proxy trim 500' (or 'proxy trim off' to clear)")
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		// --dry-run shows the plan: run the provider in preview (lists what would
		// be shed) without touching state or prompting.
		if dryRun {
			return providerSubcommand(p, append(append([]string{"proxy", "trim"}, positionals...), "--preview")...)
		}
		ok, err := confirmGate(fmt.Sprintf("trim %s to %s running proxies", providerLabel(p), positionals[0]), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil // declined
		}
		return providerSubcommand(p, append([]string{"proxy", "trim"}, positionals...)...)

	case "paste":
		// Paste proxies from stdin/file: normalizes formats, fetches URL sources, adds + refreshes.
		// Single-target — the paste targets one provider at a time.
		if all || len(include) > 0 || len(exclude) > 0 {
			return fmt.Errorf("proxy paste operates on ONE provider — use --unit/--user/--network to target it")
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		if dryRun {
			fmt.Printf("[dry-run] would paste proxies on %s (reads stdin/%s then proxy add + refresh)\n",
				providerLabel(p), strings.Join(positionals, " "))
			return nil
		}
		// Pass through all args (positionals + flags like --file=<path>)
		pasteArgs := []string{"proxy", "paste"}
		pasteArgs = append(pasteArgs, positionals...)
		return providerSubcommand(p, pasteArgs...)

	case "health", "traffic", "ids", "remove-dead":
		// These are single-target subcommands (selectTarget, not
		// selectTargets) — batch flags are meaningless here and must not be
		// silently dropped.
		if all || len(include) > 0 || len(exclude) > 0 {
			return fmt.Errorf("proxy %s operates on ONE provider — --all/--include/--exclude do not apply; use --unit/--user/--network to target it", sub)
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		switch sub {
		case "health":
			// State-file based; not a provider-binary delegation. Uses the
			// already-parsed target (t) — re-parsing here would see an arg
			// list with the target flags already stripped.
			return cmdProxyHealthTarget(p)
		case "traffic":
			return cmdProxyTrafficTarget(p)
		case "ids":
			return cmdProxyIDsTarget(p)
		default: // remove-dead
			if dryRun {
				fmt.Printf("[dry-run] would remove dead/degraded proxies on %s\n", providerLabel(p))
				return nil
			}
			return providerSubcommand(p, append([]string{"proxy", "remove-dead"}, positionals...)...)
		}
	case "audit":
		if all || len(include) > 0 || len(exclude) > 0 {
			return fmt.Errorf("proxy audit operates on ONE provider — use --unit/--user/--network to target it")
		}
		p, err := selectTarget(providers, t)
		if err != nil {
			return err
		}
		return cmdProxyAuditTarget(p, positionals, dryRun)
	default:
		return fmt.Errorf("unknown proxy subcommand %q (add|paste|clear|health|traffic|ids|refresh|remove-dead|add-source|remove-source|trim|audit)", sub)
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
			fmt.Printf("  %s (user=%s, network=%s)\n", providerLabel(p), p.User, p.netLabel())
		}
		return nil
	}

	var perProviderErrs []string
	for _, p := range chosen {
		if err := providerSubcommand(p, opArgs...); err != nil {
			// Continue past per-provider failures so a single bad
			// provider can't strand the rest of the batch (audit M16:
			// `proxy clear --all` previously stopped on the first
			// failure and left downstream providers untouched with no
			// summary). Surface the failure at the end so it is not
			// silently swallowed.
			perProviderErrs = append(perProviderErrs, fmt.Sprintf("%s: %v", providerLabel(p), err))
			continue
		}
	}
	if len(perProviderErrs) > 0 {
		return fmt.Errorf("%d of %d provider(s) failed: %s", len(perProviderErrs), len(chosen), strings.Join(perProviderErrs, "; "))
	}
	return nil
}

// expandHomePath expands leading ~/ or ~\ to the user's home directory.
func expandHomePath(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// shortDuration renders d as "1h30m" or "45m", never negative.
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// formatProxyAuditStatus renders the proxy audit status as status-command lines.
func formatProxyAuditStatus(as *ProxyAuditStatus, now time.Time) []string {
	if as == nil {
		return nil
	}
	var lines []string
	if !as.Acting {
		if as.WouldPark > 0 {
			advice := "turn proxy audit on to act: urnet-tools proxy audit on"
			switch as.NotActingReason {
			case "audit-off":
				advice = "proxy audit is off; turn it on to act: urnet-tools proxy audit on"
			case "hot-restart-off":
				advice = "proxy audit is on, but parking also needs hot restart: urnet-tools hot-restart on"
			}
			lines = append(lines, fmt.Sprintf("proxy audit: observing only, %d would be parked (%s)", as.WouldPark, advice))
		} else {
			lines = append(lines, "proxy audit: observing only, nothing would be parked")
		}
	} else {
		parked := "none parked"
		if len(as.Parked) > 0 {
			parked = fmt.Sprintf("%d parked", len(as.Parked))
		}
		lines = append(lines, fmt.Sprintf("proxy audit: acting, %s, %d parks in the last 24h", parked, as.Parks24h))
	}
	if as.Paused {
		lines = append(lines, fmt.Sprintf("proxy audit: PAUSED for %s: the paid proxy list is unreadable or empty, so nothing is parked until it can be read again", shortDuration(now.Sub(as.PausedSince))))
	}
	for _, p := range as.Parked {
		remaining := p.Until.Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		who := p.Addr
		if p.User != "" {
			who = fmt.Sprintf("%s (user %s)", p.Addr, p.User)
		}
		lines = append(lines, fmt.Sprintf("  parked %s for another %s", who, shortDuration(remaining)))
	}
	if as.Distrusted {
		lines = append(lines, "  last pass distrusted: a large share of grades went bad at once, so nothing was parked")
	}
	if as.Thin {
		lines = append(lines, "  last pass thin: too few proxies could be graded from this box, so nothing was parked")
	}
	return lines
}

// cmdProxyAuditTarget executes a proxy audit command on a single targeted provider.
func cmdProxyAuditTarget(p Provider, positionals []string, dryRun bool) error {
	action := "status"
	if len(positionals) > 0 {
		action = positionals[0]
	}

	switch action {
	case "status":
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "audit", Action: "status"})
		if err != nil || !resp.OK {
			if resp2, err2 := sendSocketRequest(sockPath, controlRequest{Cmd: "status"}); err2 == nil && resp2.OK {
				resp = resp2
			} else {
				if err != nil {
					return fmt.Errorf("connect to provider on %s: %w", sockPath, err)
				}
				return fmt.Errorf("provider rejected audit status on %s: %s", sockPath, resp.Error)
			}
		}
		as := resp.ProxyAudit
		if as == nil {
			as = resp.Audit
		}
		lines := formatProxyAuditStatus(as, time.Now())
		if len(lines) == 0 {
			fmt.Println("proxy audit: observing only, nothing to report yet")
			return nil
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil

	case "on":
		if dryRun {
			fmt.Printf("[dry-run] would enable proxy audit on %s\n", providerLabel(p))
			return nil
		}
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "audit", Action: "on"})
		if err != nil || !resp.OK {
			applied, _, cErr := applyControlOverride(p, "set", "proxy_audit", "on", false)
			if cErr != nil {
				return fmt.Errorf("enable proxy audit on %s: %w", providerLabel(p), cErr)
			}
			if !applied {
				fmt.Printf("✓ Proxy audit enabled for %s (queued; takes effect on next provider start)\n", providerLabel(p))
				return nil
			}
		} else {
			_ = queuePendingOverrideIn(p.StateHome, p.StateDir, "set", "proxy_audit", "on")
		}
		fmt.Printf("✓ Proxy audit enabled for %s (evaluates proxy quality every 5m; parks junk paid/file proxies)\n", providerLabel(p))
		return nil

	case "off":
		if dryRun {
			fmt.Printf("[dry-run] would disable proxy audit on %s\n", providerLabel(p))
			return nil
		}
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "audit", Action: "off"})
		if err != nil || !resp.OK {
			applied, _, cErr := applyControlOverride(p, "set", "proxy_audit", "off", false)
			if cErr != nil {
				return fmt.Errorf("disable proxy audit on %s: %w", providerLabel(p), cErr)
			}
			if !applied {
				fmt.Printf("✓ Proxy audit disabled for %s (queued; takes effect on next provider start)\n", providerLabel(p))
				return nil
			}
		} else {
			_ = queuePendingOverrideIn(p.StateHome, p.StateDir, "set", "proxy_audit", "off")
		}
		fmt.Printf("✓ Proxy audit disabled for %s (observe mode only; any parked proxies released)\n", providerLabel(p))
		return nil

	case "release":
		if len(positionals) < 2 {
			return fmt.Errorf("proxy audit release requires a proxy address (e.g. 'urnet-tools proxy audit release 1.2.3.4:1080') or '--all'")
		}
		targetAddr := positionals[1]
		if targetAddr == "all" {
			targetAddr = "--all"
		}
		if dryRun {
			if targetAddr == "--all" {
				fmt.Printf("[dry-run] would release all parked proxies on %s\n", providerLabel(p))
			} else {
				fmt.Printf("[dry-run] would release parked proxy %s on %s\n", targetAddr, providerLabel(p))
			}
			return nil
		}
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "audit", Action: "release", Address: targetAddr})
		if err != nil {
			return fmt.Errorf("release on %s: %w", providerLabel(p), err)
		}
		if !resp.OK {
			return fmt.Errorf("release on %s: %s", providerLabel(p), resp.Error)
		}
		if targetAddr == "--all" {
			fmt.Printf("✓ Released all parked proxies on %s (%s)\n", providerLabel(p), resp.Value)
		} else {
			fmt.Printf("✓ Released proxy %s on %s (%s)\n", targetAddr, providerLabel(p), resp.Value)
		}
		return nil

	default:
		return fmt.Errorf("unknown proxy audit action %q (status|on|off|release)", action)
	}
}
