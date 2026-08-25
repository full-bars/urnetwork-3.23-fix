package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// rootHint returns a copy-pasteable "see every provider" suggestion, or ""
// when it doesn't apply (already root, or a platform without sudo). Plain
// `sudo urnet-tools` doesn't work: the binary installs to a per-user path
// (~/.local/share/urnetwork-provider/bin), never onto root's $PATH, so root
// has no "urnet-tools" to find. os.Executable() resolves the actual path
// this process is running from so the hint is directly runnable.
func rootHint() string {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return "sudo " + exe
}

// printNarrowedNote reports that selectTargetOrSoleAccessible auto-picked
// the sole provider reachable without root, so the operator knows other
// providers exist on the box but were skipped rather than acted on — same
// wording across every read-only command that uses the narrowing (logs,
// status, summary), so the behavior reads as one consistent tool feature
// rather than a per-command surprise.
func printNarrowedNote(totalFound int, p Provider, what string) {
	note := fmt.Sprintf("Note: %d providers found; only user=%s is accessible without root — showing its %s.", totalFound, p.User, what)
	if hint := rootHint(); hint != "" {
		note += fmt.Sprintf(" To see/target the others: %s %s", hint, what)
	}
	fmt.Println(note)
}

// printLifecycleNarrowedNote is the ACTION variant of printNarrowedNote for
// start/stop/restart: the sole-accessible auto-pick must be loud on a
// mutating command (LA1 6b — recovery failed because the operator could not
// tell whether anything had been targeted at all).
func printLifecycleNarrowedNote(totalFound int, p Provider, action string) {
	// Explicit gerund map: action+"ting" would yield "restartting" for
	// restart (CR #4 typo). Unknown actions fall back to the old heuristic.
	gerund := map[string]string{
		"stop":    "stopping",
		"start":   "starting",
		"restart": "restarting",
	}[action]
	if gerund == "" {
		gerund = action + "ting"
	}
	note := fmt.Sprintf("Note: %d providers found; only user=%s is accessible without root — %s it.", totalFound, p.User, gerund)
	if hint := rootHint(); hint != "" {
		note += fmt.Sprintf(" To target another provider: %s --unit <unit>", hint)
	}
	fmt.Println(note)
}

// selectTargets resolves a provider list against targeting criteria and
// returns the chosen set (one or more providers). It is the batch analogue
// of selectTarget.
//
// Resolution order:
//  1. include (--include a,b) selects exactly those providers; ambiguous
//     entries are an error.
//  2. interactive (--select) prompts with a numbered list; the operator
//     picks entries by number (comma/space separated) or "all".
//  3. no criteria: single provider → that one; multiple → refuse with the
//     inventory (same guard as selectTarget).
//
// exclude (--exclude) subtracts providers from whatever set was chosen; it
// never expands a set.
func selectTargets(providers []Provider, t Target, include, exclude []string, interactive bool) ([]Provider, error) {
	var chosen []Provider
	var err error

	switch {
	case t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "":
		// A single explicit target still resolves to one provider.
		p, serr := selectTarget(providers, t)
		if serr != nil {
			return nil, serr
		}
		chosen = []Provider{p}
	case len(include) > 0:
		chosen, err = selectByLabels(providers, include)
		if err != nil {
			return nil, err
		}
	case interactive:
		chosen, err = interactivePick(providers)
		if err != nil {
			return nil, err
		}
	case len(providers) == 1:
		chosen = providers
	case len(providers) == 0:
		return nil, fmt.Errorf("no providers found on this box")
	default:
		// An explicitly persisted default provider (default set) resolves the
		// no-target case with multiple providers, before any other heuristic.
		// It only fills the "no target at all" gap: explicit --all/--include/
		// and target flags were already handled above, so they always win.
		if p, ok := resolveDefaultProvider(providers); ok {
			// Visible trace that a stored default decided this (not an explicit
			// flag) — same reason as selectTarget.
			fmt.Fprintf(os.Stderr, "using persisted default provider: %s\n", providerLabel(p))
			chosen = []Provider{p}
			break
		}
		// Restore the pre-multi-provider default for unprivileged callers:
		// act on the single running provider for the current user. Root
		// falls through to the inventory refusal (root can act on all
		// providers). Refuse when the default is genuinely ambiguous (two
		// or more running providers for the current user).
		var defaultReason string
		if !isPrivileged() {
			if p, err := defaultProvider(providers); err == nil {
				chosen = []Provider{p}
				break
			} else {
				defaultReason = err.Error()
			}
		}
		return nil, ambiguousErrorWithReason(providers, defaultReason)
	}

	if len(exclude) > 0 {
		excluded := labelSet(exclude)
		// Build a NEW slice — filtering in place (chosen[:0]) would write
		// through to the caller's backing array when chosen aliases
		// providers (single-provider default path), mutating the input
		// (free-review major).
		filtered := make([]Provider, 0, len(chosen))
		for _, p := range chosen {
			if !excluded[matchKey(p)] && !excluded[p.Unit] && !excluded[p.Network] {
				filtered = append(filtered, p)
			}
		}
		chosen = filtered
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("selection is empty after applying criteria")
	}
	return chosen, nil
}

// selectTargetOrSoleAccessible behaves like selectTarget for a read-only
// command (logs), except: with no explicit target, multiple providers
// discovered, and the caller unprivileged (not root, who can reach all of
// them), it narrows to the providers actually reachable without root
// (narrowToAccessible) and auto-picks the result if exactly one remains,
// rather than refusing with the "N providers found" ambiguity guard — the
// other N-1 are running under accounts the caller has no way to select
// correctly anyway (see discover_unix.go ghost-provider fix). The guard
// still applies whenever more than one provider is actually reachable, or
// when root is asking (who CAN reach all of them and should get the normal
// refusal + inventory).
func selectTargetOrSoleAccessible(providers []Provider, t Target) (p Provider, narrowed bool, err error) {
	noTarget := t.Unit == "" && t.User == "" && t.Network == "" && t.NetworkID == "" && t.StateDir == ""
	// Use the platform privilege seam (isPrivileged), not os.Geteuid() direct:
	// Geteuid is meaningless on Windows (returns -1) where an elevated Admin
	// should still get the normal refusal + inventory, not the unprivileged
	// auto-narrow treatment. Matches target.go and select_multi.go:93.
	if noTarget && len(providers) > 1 && !isPrivileged() {
		if accessible := narrowToAccessible(providers); len(accessible) == 1 {
			return accessible[0], true, nil
		}
	}
	p, err = selectTarget(providers, t)
	return p, false, err
}

// selectByLabels matches providers by unit name, user, or network name —
// the labels shown in the inventory table. Each label must match exactly
// one provider.
func selectByLabels(providers []Provider, labels []string) ([]Provider, error) {
	var out []Provider
	seen := map[string]bool{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		var matches []Provider
		for _, p := range providers {
			if p.Unit == label || p.User == label || p.Network == label || matchKey(p) == label {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("label %q matches no provider", label)
		case 1:
			if !seen[matchKey(matches[0])] {
				out = append(out, matches[0])
				seen[matchKey(matches[0])] = true
			}
		default:
			return nil, fmt.Errorf("label %q is ambiguous (%d matches); use unit or network name", label, len(matches))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no providers matched the given labels")
	}
	return out, nil
}

// selectTargetInteractive resolves ONE provider for a read-only command:
// an explicit target resolves strictly (never prompts), the sole provider is
// auto-selected without prompting, and multiple providers with no target pop
// the interactive picker.
func selectTargetInteractive(providers []Provider, t Target) (Provider, error) {
	if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
		return selectTarget(providers, t)
	}
	switch len(providers) {
	case 0:
		return Provider{}, fmt.Errorf("no providers found on this box")
	case 1:
		return providers[0], nil
	default:
		chosen, err := selectTargets(providers, t, nil, nil, true)
		if err != nil {
			return Provider{}, err
		}
		if len(chosen) != 1 {
			return Provider{}, fmt.Errorf("read-only command requires exactly one provider, got %d", len(chosen))
		}
		return chosen[0], nil
	}
}

// interactivePick shows a numbered list and prompts the operator to choose
// entries (comma/space separated numbers, "all", or empty for none).
func interactivePick(providers []Provider) ([]Provider, error) {
	fmt.Println("Select providers (comma/space separated numbers, or 'all'):")
	for i, p := range providers {
		fmt.Printf("  [%d] %s  user=%s  net=%s  state=%s\n",
			i+1, providerLabel(p), p.User, p.Network, p.StateDir)
	}
	line, err := confirmStdinRead("> ")
	if err != nil {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("no providers selected")
	}
	if strings.EqualFold(line, "all") {
		return providers, nil
	}
	var out []Provider
	seen := map[int]bool{}
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(providers) {
			return nil, fmt.Errorf("invalid selection %q", tok)
		}
		if !seen[n-1] {
			out = append(out, providers[n-1])
			seen[n-1] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no providers selected")
	}
	return out, nil
}

// labelSet builds a lookup set from exclude labels for quick membership.
func labelSet(labels []string) map[string]bool {
	m := map[string]bool{}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l != "" {
			m[l] = true
		}
	}
	return m
}

// matchKey returns a stable unique key for a provider. The state dir is the
// only field guaranteed unique per provider (two providers CAN share the
// same network name — e.g. same account on mainnet and beta backends — and
// even the same user). Unit is preferred when present for readability, but
// state dir breaks ties.
func matchKey(p Provider) string {
	if p.Unit != "" {
		// Unit names are unique per box, but a user-level unit could exist
		// for multiple users; state dir disambiguates.
		if p.StateDir != "" {
			return p.Unit + "|" + p.StateDir
		}
		return p.Unit
	}
	if p.StateDir != "" {
		return p.StateDir
	}
	return p.User + "@" + p.Network
}

// ambiguousErrorWithReason is ambiguousError plus the reason the default
// selection could not apply, when one exists. Keeps the batch paths (which
// route through selectTargets) as informative as the single-target path.
func ambiguousErrorWithReason(providers []Provider, reason string) error {
	err := ambiguousError(providers)
	if reason != "" {
		return fmt.Errorf("%s(%s)", strings.TrimRight(err.Error(), "\n"), reason)
	}
	return err
}

// ambiguousError renders the refusal message with the inventory.
func ambiguousError(providers []Provider) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d providers found — specify a target (--unit / --user / --network / --state-dir) or --include/--select:\n", len(providers))
	for _, p := range providers {
		fmt.Fprintf(&b, "  %s  user=%s  net=%s  state=%s\n", providerLabel(p), p.User, p.Network, p.StateDir)
	}
	if hint := rootHint(); hint != "" {
		fmt.Fprintf(&b, "some of these may belong to other accounts you can't see fully without root; to inspect all of them: %s\n", hint)
	}
	return fmt.Errorf("%s", b.String())
}
