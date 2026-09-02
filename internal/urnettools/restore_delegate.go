package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
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
		file := filepath.Join(p.StateDir, "fast_auth")
		if _, err := os.Stat(file); err == nil {
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
	"  node-name           <string>    node name reported to the fleet hub (default: hostname)",
	"  report-interval     <duration>  bandwidth report cadence (default: 5m, min: 10s)",
	"  proxy-url-max       <int>       max proxies from URL feeds (default: 500)",
	"  proxy-url-refresh   <duration>  URL proxy list refresh interval (default: 1h, min: 10s)",
	"  cleanup-scope       none|url|all  dead proxy auto-cleanup scope (default: url)",
	"  cleanup-interval    <duration>  dead proxy cleanup interval (default: 6h, min: 1m)",
	"  fast-auth           on|off      bypass auth rate limiter (marker file)",
}

func printSetHelp() {
	fmt.Fprint(os.Stderr, `urnet-tools set — runtime tuning overrides

Usage: urnet-tools set <key> [<value>|off] [target]

Runtime overrides are files the provider reads live from ~/.urnetwork/.
Changes take effect on the next provider tick (no restart needed).
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
`)
}

// cmdSet restores `urnet-tools set <key> [<value>|off] [target]`: it reads and
// writes the provider's runtime-override files in ~/.urnetwork. The provider
// consumes these at runtime, so no restart is needed.
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

	// No key: list every active override, mirroring the legacy do_set.
	if len(rest) == 0 {
		return formatSets(p, "")
	}
	key := rest[0]
	// key only: show current value.
	if len(rest) == 1 {
		filename, ok := setKeyFiles[key]
		if !ok {
			return fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
		}
		return formatSets(p, filepath.Join(p.StateDir, filename))
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
// on a resolved provider. Extracted from cmdSet so the file lifecycle is
// directly unit-testable with a temp state dir (no live discovery). It
// validates the key against setKeyFiles and routes fast-auth to the marker
// logic. Honours --dry-run.
func applySetOverride(p Provider, key, value string, dryRun bool) error {
	filename, ok := setKeyFiles[key]
	if !ok {
		return fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	// fast-auth is existence-based, not value-based — manage the marker file
	// directly so the message and the on/off semantics match do_fast_auth.
	if filename == "fast_auth" {
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

	if err := validateSetValue(key, value); err != nil {
		return err
	}

	file := filepath.Join(p.StateDir, filename)
	if value == "off" {
		if dryRun {
			fmt.Printf("[dry-run] would clear %s ('%s') and revert to startup default\n", file, key)
			return nil
		}
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("%s cleared for %s — reverts to startup default on next tick\n", key, providerLabel(p))
		return nil
	}

	if dryRun {
		fmt.Printf("[dry-run] would set %s=%s for %s (%s)\n", key, value, providerLabel(p), file)
		return nil
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return err
	}
	// When run as root, MkdirAll creates the dir owned by root. Fix
	// ownership so the provider process can write its own state.
	// Uses Lchown to prevent following symlinks (C2 fix).
	if uid, gid, _ := lookupUserIDs(p.User); uid >= 0 {
		_ = chownStateDir(p.StateDir, uid, gid)
	}
	// 0o644, matching the sibling override-writers: the provider often runs
	// under a different user than the tool, so a 0600 file would be unreadable
	// and the change would silently never take effect.
	// Uses writeStateFile (O_NOFOLLOW) to prevent symlink-following attacks.
	if err := writeStateFile(p.StateDir, filename, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %v", file, err)
	}
	// chown the written file so the provider can read/rewrite it.
	// Uses Lchown to prevent following symlinks.
	if uid, gid, _ := lookupUserIDs(p.User); uid >= 0 {
		_ = chownStateFile(file, uid, gid)
	}
	fmt.Printf("%s set to %s for %s — takes effect on next provider tick\n", key, value, providerLabel(p))
	return nil
}

// setFastAuthMarker sets or clears the auth-rate-limiter bypass marker in the
// provider's state dir, honouring --dry-run. Shared by `set fast-auth <v>` and
// the standalone fast-auth command path semantics.
func setFastAuthMarker(p Provider, on bool, dryRun bool) error {
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	file := filepath.Join(p.StateDir, "fast_auth")
	if !on {
		if dryRun {
			fmt.Printf("[dry-run] would disable fast-auth for %s (remove %s)\n", providerLabel(p), file)
			return nil
		}
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("fast-auth: off for %s — auth rate limiter active\n", providerLabel(p))
		return nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would enable fast-auth for %s (marker %s)\n", providerLabel(p), file)
		return nil
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return err
	}
	if uid, gid, _ := lookupUserIDs(p.User); uid >= 0 {
		_ = chownStateDir(p.StateDir, uid, gid)
	}
	if err := writeStateFile(p.StateDir, "fast_auth", nil, 0o644); err != nil {
		return err
	}
	if uid, gid, _ := lookupUserIDs(p.User); uid >= 0 {
		_ = chownStateFile(file, uid, gid)
	}
	fmt.Printf("fast-auth: on for %s — auth rate limiter bypassed (effective immediately)\n", providerLabel(p))
	return nil
}

// formatSets prints active runtime overrides from the provider's state dir.
// When want is non-nil it prints only that one file's status; otherwise it
// lists every override in canonical key order. Mirrors legacy do_set.
func formatSets(p Provider, want string) error {
	keyFor := func(base string) string {
		for k, f := range setKeyFiles {
			if f == base {
				return k
			}
		}
		return ""
	}
	if want != "" {
		key := keyFor(filepath.Base(want))
		b, err := os.ReadFile(want)
		if os.IsNotExist(err) {
			if key == "fast-auth" {
				fmt.Printf("fast-auth: off for %s (not set)\n", providerLabel(p))
			} else {
				fmt.Printf("%s: not set for %s (using startup default)\n", key, providerLabel(p))
			}
			return nil
		}
		if err != nil {
			return err
		}
		if key == "fast-auth" {
			fmt.Printf("fast-auth: on for %s\n", providerLabel(p))
		} else {
			fmt.Printf("%s: %s\n", key, strings.TrimSpace(string(b)))
		}
		return nil
	}

	fmt.Printf("Runtime overrides (%s/):\n", p.StateDir)
	found := 0
	for _, k := range []string{"node-name", "report-interval", "proxy-url-max", "proxy-url-refresh", "cleanup-scope", "cleanup-interval", "fast-auth"} {
		f := filepath.Join(p.StateDir, setKeyFiles[k])
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		found++
		if k == "fast-auth" {
			fmt.Printf("  %-32s %s\n", setKeyFiles[k], "on")
		} else {
			fmt.Printf("  %-32s %s\n", setKeyFiles[k], strings.TrimSpace(string(b)))
		}
	}
	if found == 0 {
		fmt.Println("No runtime overrides set (all using startup defaults).")
	}
	return nil
}
