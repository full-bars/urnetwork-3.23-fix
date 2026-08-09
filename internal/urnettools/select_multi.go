package urnettools

import (
	"fmt"
	"strconv"
	"strings"
)

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
		return nil, ambiguousError(providers)
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

// interactivePick shows a numbered list and prompts the operator to choose
// entries (comma/space separated numbers, "all", or empty for none).
func interactivePick(providers []Provider) ([]Provider, error) {
	fmt.Println("Select providers (comma/space separated numbers, or 'all'):")
	for i, p := range providers {
		fmt.Printf("  [%d] %s  user=%s  net=%s  state=%s\n",
			i+1, providerLabel(p), p.User, p.Network, p.StateDir)
	}
	fmt.Print("> ")
	line, err := stdinReader.ReadString('\n')
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

// ambiguousError renders the refusal message with the inventory.
func ambiguousError(providers []Provider) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d providers found — specify a target (--unit / --user / --network / --state-dir) or --include/--select:\n", len(providers))
	for _, p := range providers {
		fmt.Fprintf(&b, "  %s  user=%s  net=%s  state=%s\n", providerLabel(p), p.User, p.Network, p.StateDir)
	}
	return fmt.Errorf("%s", b.String())
}
