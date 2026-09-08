package urnettools

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file restores urnet-tools commands that the Go rewrite STRIPPED from
// the legacy shell tool (Provider_Install_Linux.sh + urnet-tools.ps1):
//
//	auth            authenticate a provider (delegates to the provider binary)
//	choose-network  switch API/connect URLs (delegates to the provider binary)
//	fast-auth       toggle the auth-rate-limiter bypass marker
//	set             read/write/clear runtime tunings in ~/.urnetwork
//
// auth and choose-network delegate to the targeted provider binary via
// providerSubcommand (the same pattern proxy/summary/hot-restart use). They
// must NOT run parseGlobalFlags first, because the provider binary's own -f
// (force-overwrite the JWT) has to reach the provider rather than being
// consumed by the tool. fast-auth and set are file operations in the
// provider's state dir (~/.urnetwork), matching the files the provider reads
// at runtime.

// stripDryRunFlag extracts -n/--dry-run from args. Used by the delegation
// commands (auth/choose-network) that must honour dry-run rather than forward
// it to a provider binary that rejects it.
func stripDryRunFlag(args []string) (bool, []string) {
	dry := false
	var rest []string
	for _, a := range args {
		if a == "-n" || a == "--dry-run" {
			dry = true
			continue
		}
		rest = append(rest, a)
	}
	return dry, rest
}

// cmdAuth restores `urnet-tools auth [<auth-code>] [target]`. It delegates
// to the targeted provider binary's `auth` subcommand, streaming output and
// error to this process. The provider already prompts to overwrite an
// existing JWT unless -f is given (provider/main.go auth()), so user flags
// including the provider's own -f are passed through untouched rather than
// the tool forcing an overwrite.
func cmdAuth(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools auth — authenticate a provider

Usage: urnet-tools auth [<auth-code>] [target]

Authenticates the targeted provider against the URnetwork API. With no auth
code it hashes the provider's stored identity. Existing credentials are
overwritten only with -f (the provider otherwise prompts to confirm).

Provider flags pass through (e.g. --api_url=...). See 'providers' to learn
each provider's --unit/--user/--network.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	// Audience trail without an extra prompt (auth is a consequential write).
	_, _ = confirmGate("authenticate "+providerLabel(p), p, true, dryRun)
	if dryRun {
		fmt.Printf("[dry-run] would delegate 'provider auth' on %s\n", providerLabel(p))
		return nil
	}
	return providerSubcommand(p, append([]string{"auth"}, rest...)...)
}

// cmdChooseNetwork restores `urnet-tools choose-network [target]` with either
// two positional URLs or --reset. It delegates to the provider binary's
// `choose_network` subcommand, which saves the API/connect URLs as the chosen
// network (or, with --reset, clears the saved network and reverts to the main
// network).
func cmdChooseNetwork(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools choose-network — set the network the provider connects to

Usage: urnet-tools choose-network <api_url> <connect_url> [target]
       urnet-tools choose-network main|beta [target]
       urnet-tools choose-network --reset [target]

Saves an API URL (http/https) and connect URL (ws/wss) as the provider's
chosen network. A preset name (main or beta) selects the built-in network
without typing URLs. --reset clears the saved network and reverts to the main
network. Delegates to the provider binary and streams its output.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	// Audience trail without an extra prompt (repointing the network is
	// consequential).
	_, _ = confirmGate("choose network "+providerLabel(p), p, true, dryRun)
	if dryRun {
		fmt.Printf("[dry-run] would delegate 'provider choose_network' on %s\n", providerLabel(p))
		return nil
	}
	return providerSubcommand(p, append([]string{"choose_network"}, rest...)...)
}

// cmdFastAuth restores `urnet-tools fast-auth on|off|status [target]`: it
// manages the ~/.urnetwork/fast_auth marker file that bypasses the provider's
// auth rate limiter. Existence of the marker is the on/off state (the
// provider treats any presence of the file as unlocked), matching the legacy
// shell do_fast_auth. Honours --dry-run and the confirm/force gate.
func cmdFastAuth(args []string, force, dryRun bool) error {
	// Parse the full arg list for target flags FIRST, then take the action
	// token from what remains. This matches cmdSet and means a target flag
	// placed before the action (e.g. `fast-auth --unit myunit on`) is handled
	// instead of the action silently dropping and the command erroring
	// misleadingly.
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return fmt.Errorf("fast-auth takes on|off|status only (got %v)", rest)
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	switch sub {
	case "on", "off":
		// Mirrors the audit-trail + confirm convention used by hub set/off and
		// the tune commands: even -f prints the target line to stderr; without
		// -f the operator must type an explicit yes.
		ok, err := confirmGate("fast-auth "+sub+" "+providerLabel(p), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil // dry-run or declined: confirmGate printed the audit line
		}
		return setFastAuthMarker(p, sub == "on", dryRun)
	case "", "status":
		val, _, found, _ := queryControlOverride(p, "fast_auth")
		if found && strings.EqualFold(val, "on") {
			fmt.Printf("fast-auth: on for %s (rate limiter bypassed)\n", providerLabel(p))
		} else {
			fmt.Printf("fast-auth: off for %s (rate limiter active) — use 'fast-auth on' to bypass\n", providerLabel(p))
		}
	default:
		// Unknown action is rejected, never silently treated as status: a typo
		// must not enable the bypass.
		return fmt.Errorf("fast-auth action must be on, off, or status (got %q)", sub)
	}
	return nil
}

// validateSetValue rejects values the provider would silently discard (it keeps
// the startup default for an unparseable/out-of-range value), so the command
// never reports an effect that provably cannot take place.
func validateSetValue(key, value string) error {
	switch key {
	case "report-interval", "proxy-url-refresh", "cleanup-interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q (use e.g. 30s, 5m, 1h)", key, value)
		}
		min := 10 * time.Second
		if key == "cleanup-interval" {
			min = time.Minute
		}
		if d < min {
			return fmt.Errorf("%s: %s is below the minimum %s", key, value, min)
		}
	case "proxy-url-max":
		if n, err := strconv.Atoi(value); err != nil || n < 0 {
			return fmt.Errorf("%s: must be a non-negative integer (got %q)", key, value)
		}
	case "cleanup-scope":
		switch value {
		case "none", "url", "all":
		default:
			return fmt.Errorf("%s: must be none, url, or all (got %q)", key, value)
		}
	}
	return nil
}

// setKeyFiles maps a user-facing `set` key to the filename the provider reads
// in its state dir. Mirrors the legacy _set_key_to_file table.
var setKeyFiles = map[string]string{
	"node-name":         "node_name",
	"report-interval":   "report_interval",
	"proxy-url-max":     "proxy_url_max",
	"proxy-url-refresh": "proxy_url_refresh",
	"cleanup-scope":     "proxy_dead_cleanup_scope",
	"cleanup-interval":  "proxy_dead_cleanup_interval",
	"fast-auth":         "fast_auth",
}

// setKeyHelps describes each key for `set help` / usage.
var setKeyHelps = []string{
	"  node-name           <string>      node name reported to the fleet hub (default: hostname)",
	"  report-url          <url>|off     bandwidth report destination URL",
	"  report-interval     <duration>    bandwidth report cadence (default: 5m, min: 10s)",
	"  fast-auth           on|off        bypass auth rate limiter",
	"  self-heal           on|off        dead proxy auto-cleanup & load gate",
	"  proxy-url-max       <int>         max proxies from URL feeds (default: 500)",
	"  proxy-url-refresh   <duration>    URL proxy list refresh interval (default: 1h, min: 10s)",
	"  cleanup-scope       none|url|all  dead proxy auto-cleanup scope (default: url)",
	"  cleanup-interval    <duration>    dead proxy cleanup interval (default: 6h, min: 1m)",
	"  hot-restart         on|off        preserve client JWTs across restarts",
	"  gomemlimit          <bytes>       Go runtime memory limit (e.g. 256MiB, 1GiB)",
	"  gogc                <int>|off     Go runtime garbage collection target percentage (default: 100)",
	"  profile             <profile>     tuning profile (auto, eco, lowmem, turbo-v4, turbo-v8)",
	"  ramlogs             on|off        in-memory ramlogs toggle",
}

func printSetHelp() {
	fmt.Fprint(os.Stderr, `urnet-tools set — runtime tuning overrides

Usage: urnet-tools set <key> [<value>|off] [target]

Runtime overrides are managed via the provider's control socket (~/.urnetwork/provider.sock).
When the provider is running, changes take effect immediately without a restart.
When the provider is not running, changes are queued in pending_overrides.json and apply on next startup.

Set a value:  urnet-tools set <key> <value>
Show current: urnet-tools set <key>
Clear it:     urnet-tools set <key> off
List all:     urnet-tools set

Available keys:
`)
	for _, l := range setKeyHelps {
		fmt.Fprintln(os.Stderr, l)
	}
	fmt.Fprint(os.Stderr, `
Duration format: Go-style, e.g. 30s, 5m, 1h, 24h.
Byte format: Go-style, e.g. 256MiB, 1GiB, 500MB.
`)
}

// cmdSet restores `urnet-tools set <key> [<value>|off] [target]`: it manages the
// provider's runtime overrides via its control socket, falling back to
// pending_overrides.json if the provider is not running.
func cmdSet(args []string, force, dryRun bool) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 && rest[0] == "help" {
		printSetHelp()
		return nil
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	// No key: list every active override.
	if len(rest) == 0 {
		return formatSets(p, "")
	}
	key := rest[0]
	// key only: show current value.
	if len(rest) == 1 {
		canonicalKey, ok := canonicalControlKey(key)
		if !ok {
			return fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
		}
		return formatSets(p, canonicalKey)
	}
	if len(rest) > 2 {
		return fmt.Errorf("set takes <key> [<value>|off] (got %v)", rest[1:])
	}
	// Gate before mutating the state dir, like the other single-target write
	// commands (audit line to stderr even under -f; explicit yes otherwise).
	ok, err := confirmGate(fmt.Sprintf("set %s on %s", key, providerLabel(p)), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run or declined: confirmGate printed the audit line
	}
	return applySetOverride(p, key, rest[1], dryRun)
}

// applySetOverride writes, clears, or shows the runtime override for one key
// on a resolved provider. It talks to the provider's control socket if reachable,
// or queues the change in pending_overrides.json if the provider is down.
func applySetOverride(p Provider, key, value string, dryRun bool) error {
	canonicalKey, ok := canonicalControlKey(key)
	if !ok {
		return fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	// fast-auth is existence-based, not value-based — manage via fast_auth helper
	if canonicalKey == "fast_auth" {
		switch value {
		case "on":
			return setFastAuthMarker(p, true, dryRun)
		case "off":
			return setFastAuthMarker(p, false, dryRun)
		default:
			// A typo must not silently enable the bypass.
			return fmt.Errorf("set fast-auth accepts 'on' or 'off' only (got %q); 'fast-auth <on|off|status>' queries status", value)
		}
	}

	if value == "off" && canonicalKey != "hot_restart" && canonicalKey != "ramlogs" && canonicalKey != "proxy_self_heal" {
		if dryRun {
			fmt.Printf("[dry-run] would clear %s for %s and revert to startup default\n", key, providerLabel(p))
			return nil
		}
		appliedLive, err := applyControlOverride(p, "clear", canonicalKey, "", false)
		if err != nil {
			return err
		}
		if appliedLive {
			fmt.Printf("%s cleared for %s (applied live via control socket)\n", key, providerLabel(p))
		} else {
			fmt.Printf("%s cleared for %s (provider not running; queued in pending_overrides.json, takes effect on next start)\n", key, providerLabel(p))
		}
		return nil
	}

	if err := validateControlValue(canonicalKey, value); err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("[dry-run] would set %s=%s for %s\n", key, value, providerLabel(p))
		return nil
	}

	appliedLive, err := applyControlOverride(p, "set", canonicalKey, value, false)
	if err != nil {
		return err
	}
	if appliedLive {
		fmt.Printf("%s set to %s for %s (applied live via control socket)\n", key, value, providerLabel(p))
	} else {
		fmt.Printf("%s set to %s for %s (provider not running; queued in pending_overrides.json, takes effect on next start)\n", key, value, providerLabel(p))
	}
	return nil
}

// setFastAuthMarker sets or clears the auth-rate-limiter bypass marker in the
// provider's control state, honouring --dry-run.
func setFastAuthMarker(p Provider, on bool, dryRun bool) error {
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	if !on {
		if dryRun {
			fmt.Printf("[dry-run] would disable fast-auth for %s\n", providerLabel(p))
			return nil
		}
		appliedLive, err := applyControlOverride(p, "clear", "fast_auth", "", false)
		if err != nil {
			return err
		}
		if appliedLive {
			fmt.Printf("fast-auth: off for %s — auth rate limiter active (applied live via control socket)\n", providerLabel(p))
		} else {
			fmt.Printf("fast-auth: off for %s — auth rate limiter active (queued in pending_overrides.json)\n", providerLabel(p))
		}
		return nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would enable fast-auth for %s\n", providerLabel(p))
		return nil
	}
	appliedLive, err := applyControlOverride(p, "set", "fast_auth", "on", false)
	if err != nil {
		return err
	}
	if appliedLive {
		fmt.Printf("fast-auth: on for %s — auth rate limiter bypassed (effective immediately via control socket)\n", providerLabel(p))
	} else {
		fmt.Printf("fast-auth: on for %s — auth rate limiter bypassed (queued in pending_overrides.json, takes effect on next start)\n", providerLabel(p))
	}
	return nil
}

// formatSets prints active runtime overrides from the provider's control state.
// When want is non-empty it prints only that one setting's status; otherwise it
// lists every override in canonical key order.
func formatSets(p Provider, want string) error {
	if want != "" {
		canonicalKey, ok := canonicalControlKey(want)
		if !ok {
			return fmt.Errorf("unknown key %q (see 'urnet-tools set help')", want)
		}
		val, _, found, err := queryControlOverride(p, canonicalKey)
		if err != nil {
			return err
		}
		if found {
			if canonicalKey == "fast_auth" {
				fmt.Printf("fast-auth: on for %s\n", providerLabel(p))
			} else {
				fmt.Printf("%s: %s\n", want, val)
			}
		} else {
			if canonicalKey == "fast_auth" {
				fmt.Printf("fast-auth: off for %s (not set)\n", providerLabel(p))
			} else {
				fmt.Printf("%s: not set for %s (using startup default)\n", want, providerLabel(p))
			}
		}
		return nil
	}

	fmt.Printf("Runtime overrides (%s/):\n", p.StateDir)
	found := 0
	orderedKeys := []struct {
		cliName      string
		canonicalKey string
	}{
		{"node-name", "node_name"},
		{"report-url", "report_url"},
		{"report-interval", "report_interval"},
		{"fast-auth", "fast_auth"},
		{"self-heal", "proxy_self_heal"},
		{"proxy-url-max", "proxy_url_max"},
		{"proxy-url-refresh", "proxy_url_refresh"},
		{"cleanup-scope", "proxy_dead_cleanup_scope"},
		{"cleanup-interval", "proxy_dead_cleanup_interval"},
		{"hot-restart", "hot_restart"},
		{"gomemlimit", "gomemlimit"},
		{"gogc", "gogc"},
		{"profile", "profile"},
		{"ramlogs", "ramlogs"},
	}

	for _, item := range orderedKeys {
		val, _, isFound, err := queryControlOverride(p, item.canonicalKey)
		if err != nil || !isFound {
			continue
		}
		found++
		if item.canonicalKey == "fast_auth" {
			fmt.Printf("  %-32s %s\n", item.cliName, "on")
		} else {
			fmt.Printf("  %-32s %s\n", item.cliName, val)
		}
	}
	if found == 0 {
		fmt.Println("No runtime overrides set (all using startup defaults).")
	}
	return nil
}
