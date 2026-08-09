package urnettools

import (
	"fmt"
	"strings"
)

// Target identifies exactly one provider to operate on. At most one field is
// set; the others are empty.
type Target struct {
	Unit      string // systemd unit name, e.g. "urnetwork-native.service"
	User      string // OS user, e.g. "urnet"
	Network   string // JWT network_name, e.g. "tacogonzalez3000" (NOT unique per box — see matchKey)
	NetworkID string // JWT network_id — the TRUE unique account identity
	StateDir  string // explicit state directory path
}

// String renders the target in a human-readable form for confirm prompts.
func (t Target) String() string {
	switch {
	case t.Unit != "":
		return fmt.Sprintf("unit %s", t.Unit)
	case t.User != "":
		return fmt.Sprintf("user %s", t.User)
	case t.NetworkID != "":
		return fmt.Sprintf("network-id %s", t.NetworkID)
	case t.Network != "":
		return fmt.Sprintf("network %s", t.Network)
	case t.StateDir != "":
		return fmt.Sprintf("state-dir %s", t.StateDir)
	default:
		return "(none)"
	}
}

// matchProvider reports whether p satisfies the target.
func (t Target) matchProvider(p Provider) bool {
	if t.Unit != "" {
		return p.Unit == t.Unit
	}
	if t.User != "" {
		return p.User == t.User
	}
	if t.NetworkID != "" {
		return p.NetworkID == t.NetworkID
	}
	if t.Network != "" {
		return p.Network == t.Network
	}
	if t.StateDir != "" {
		return p.StateDir == t.StateDir
	}
	return false
}

// selectTarget resolves the providers list against a target (or no target).
//
// Rules:
//   - An explicit target must match exactly one provider; zero or multiple
//     matches is an error.
//   - No target: if exactly one provider exists it is returned (safe
//     default); if zero providers exist that is an error; if MULTIPLE
//     providers exist the operation is REFUSED with the inventory listed —
//     the operator must say which one (the incident-class guard).
func selectTarget(providers []Provider, t Target) (Provider, error) {
	if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
		var matches []Provider
		for _, p := range providers {
			if t.matchProvider(p) {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return Provider{}, fmt.Errorf("target %s matches no running provider", t)
		default:
			return Provider{}, fmt.Errorf("target %s is ambiguous (%d matches); use a more specific target", t, len(matches))
		}
	}

	switch len(providers) {
	case 0:
		return Provider{}, fmt.Errorf("no providers found on this box")
	case 1:
		return providers[0], nil
	default:
		// The guard: multiple providers and no target = refuse.
		var b strings.Builder
		fmt.Fprintf(&b, "%d providers found — specify a target (--unit / --user / --network / --network-id / --state-dir):\n", len(providers))
		for _, p := range providers {
			fmt.Fprintf(&b, "  %s  user=%s net=%s state=%s\n", providerLabel(p), p.User, p.Network, p.StateDir)
		}
		return Provider{}, fmt.Errorf("%s", b.String())
	}
}

// providerLabel renders a short identifier for a provider in messages.
func providerLabel(p Provider) string {
	if p.Unit != "" {
		return p.Unit
	}
	if p.Network != "" {
		return fmt.Sprintf("%s@%s", p.User, p.Network)
	}
	return p.StateDir
}
