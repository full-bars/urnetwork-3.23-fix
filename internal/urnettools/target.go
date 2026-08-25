package urnettools

import (
	"fmt"
	"os"
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

// isPrivileged reports whether the caller can act on another user's provider.
// On unix this is euid==0 (root). On Windows it is a real elevation check
// (administrator token), NOT "always true": ordinary Windows users must get
// the auto-default just like unprivileged unix users. It is a package var
// seam so tests can exercise both sides without the real privilege state
// (the readEnviron seam pattern).
var isPrivileged = platformIsPrivileged

// defaultProvider resolves the "old tool" default for the no-target case:
// the single RUNNING provider for the CURRENT OS user. This restores the
// pre-multi-provider behavior where status/logs/update/etc. just acted on
// "the" provider on the box. It refrains (returns an error) only when:
//   - zero running providers belong to the current user (fall back handled
//     by the caller), OR
//   - two or more running providers belong to the current user (the
//     genuine ambiguity guard — same-user providers are indistinguishable
//     by OS account, so the operator must name one).
//
// Providers owned by OTHER users are ignored: the caller (an unprivileged
// operator) could not act on them anyway without root, and the old tool
// never saw them.
func defaultProvider(providers []Provider) (Provider, error) {
	current := currentUserName()
	var mine []Provider
	for _, p := range providers {
		if p.User != "" && p.User == current && p.Running {
			mine = append(mine, p)
		}
	}
	switch len(mine) {
	case 0:
		return Provider{}, fmt.Errorf("no running provider for user %q on this box; specify a target (--unit / --user / --network / --network-id / --state-dir)", current)
	case 1:
		return mine[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d providers found for user %q — specify a target to disambiguate (--unit / --network / --network-id):\n", len(mine), current)
		for _, p := range mine {
			fmt.Fprintf(&b, "  %s  net=%s state=%s\n", providerLabel(p), p.Network, p.StateDir)
		}
		return Provider{}, fmt.Errorf("%s", b.String())
	}
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

	var defaultReason string
	switch len(providers) {
	case 0:
		return Provider{}, fmt.Errorf("no providers found on this box")
	case 1:
		return providers[0], nil
	default:
		// An explicitly persisted default provider (default set) resolves the
		// no-target case with multiple providers, before the ambiguity refusal.
		if p, ok := resolveDefaultProvider(providers); ok {
			// Make it visible that a PERSISTED default (not an explicit flag)
			// drove the selection — under root/automation this must not read as
			// a plain single-provider auto-select (audit review finding).
			fmt.Fprintf(os.Stderr, "using persisted default provider: %s\n", providerLabel(p))
			return p, nil
		}
		// Restore the pre-multi-provider default for UNPRIVILEGED callers:
		// act on the single running provider for the current user. Root can
		// act on every provider on the box, so root always falls through to
		// the full inventory refusal instead of silently auto-picking (same
		// contract as selectTargetOrSoleAccessible). Only when two or more
		// running providers belong to the current user does an unprivileged
		// caller get refused (that is the genuine ambiguity to resolve).
		if !isPrivileged() {
			if p, err := defaultProvider(providers); err == nil {
				return p, nil
			} else {
				// Keep the reason the default failed visible in the
				// inventory refusal below.
				defaultReason = err.Error()
			}
		}
		// No unambiguous current-user provider. Fall back to the inventory
		// refusal so the operator can see everything on the box.
		var b strings.Builder
		fmt.Fprintf(&b, "%d providers found — specify a target (--unit / --user / --network / --network-id / --state-dir):\n", len(providers))
		if defaultReason != "" {
			fmt.Fprintf(&b, "(%s)\n", defaultReason)
		}
		for _, p := range providers {
			network := p.Network
			if p.IdentityRestricted {
				network = "(unreadable: permission denied)"
			}
			fmt.Fprintf(&b, "  %s  user=%s net=%s state=%s\n", providerLabel(p), p.User, network, p.StateDir)
		}
		if hint := rootHint(); hint != "" {
			fmt.Fprintf(&b, "some of these may belong to other accounts you can't see fully without root; to inspect all of them: %s\n", hint)
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
