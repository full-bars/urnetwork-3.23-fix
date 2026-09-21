// self_heal.go — restore the `urnet-tools self-heal on|off|status` command
// that the Go rewrite dropped (it existed in the pre-rewrite shell/ps1 tools
// and is still read by the provider at ~/.urnetwork/proxy_self_heal). This is
// a thin, faithful port of the old behavior: toggle or read the marker file
// the provider's self-heal gate already consumes.

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdSelfHeal toggles or reports the provider's self-heal marker file.
//
//	urnet-tools self-heal on       enable (load gate + auto cleanup)
//	urnet-tools self-heal off      disable
//	urnet-tools self-heal status   report current state
func cmdSelfHeal(args []string) error {
	mode := "status"
	rest := args
	if len(args) > 0 {
		switch args[0] {
		case "on", "off", "status":
			mode = args[0]
			rest = args[1:]
		case "-h", "--help":
			usage()
			return nil
		default:
			return fmt.Errorf("unknown self-heal sub-arg %q (on|off|status)", args[0])
		}
	}
	switch mode {
	case "on", "off":
		return writeSelfHeal(mode, rest)
	case "status":
		return showSelfHeal(rest)
	default:
		return fmt.Errorf("unknown self-heal sub-arg %q (on|off|status)", mode)
	}
}

// selfHealMarkerPath returns the provider's state dir + proxy_self_heal.
// Routes through standard target resolution so the marker lands in the
// correct provider's state dir, not the invoking user's $HOME.
func selfHealMarkerPath(p Provider) (string, error) {
	if p.StateDir == "" {
		return "", fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	return filepath.Join(p.StateDir, "proxy_self_heal"), nil
}

// selfHealPath returns the marker path, falling back to the legacy
// $HOME/.urnetwork/proxy_self_heal when no target flags are given.
// This preserves the pre-H6 behavior: `self-heal status` works without
// any provider discovered on the box. Provider-scoped self-heal requires
// an explicit target.
func selfHealPath(targetArgs []string) (string, error) {
	if len(targetArgs) == 0 {
		home := os.Getenv("HOME")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		if home == "" {
			return "", fmt.Errorf("cannot resolve self-heal marker path: $HOME is not set")
		}
		return filepath.Join(home, ".urnetwork", "proxy_self_heal"), nil
	}
	t, _, err := parseTargetFlags(targetArgs)
	if err != nil {
		return "", err
	}
	p, err := selectTarget(lifecycleCandidates(t), t)
	if err != nil {
		return "", err
	}
	return selfHealMarkerPath(p)
}

// selfHealPathProvider resolves the target provider from targetArgs.
// Returns nil when no target is given (legacy path).
func selfHealPathProvider(targetArgs []string) (*Provider, error) {
	if len(targetArgs) == 0 {
		return nil, nil
	}
	t, _, err := parseTargetFlags(targetArgs)
	if err != nil {
		return nil, err
	}
	p, err := selectTarget(lifecycleCandidates(t), t)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func writeSelfHeal(state string, targetArgs []string) error {
	// Resolve provider once (not twice via selfHealPathProvider + selfHealPath).
	// When no target is given, p is nil and we fall through to the plain
	// os.WriteFile path.
	p, perr := selfHealPathProvider(targetArgs)
	if perr != nil {
		return perr
	}
	// A provider-scoped target must resolve a real state dir. If the resolved
	// provider has an empty StateDir, falling through to the legacy $HOME path
	// would report success while not changing the selected provider (CR).
	if targetArgs != nil && p != nil && p.StateDir == "" {
		return fmt.Errorf("self-heal: provider %s has no resolvable state dir", providerLabel(*p))
	}

	// The marker is always proxy_self_heal inside the state dir.
	const markerName = "proxy_self_heal"

	if p != nil && p.StateDir != "" {
		// Write through a descriptor-pinned handle (the state dir itself is
		// opened O_NOFOLLOW): the marker and its ownership are set relative
		// to ONE open directory, so a provider user who swaps the state dir
		// for a symlink can no longer redirect the write or hand it chown
		// authority over another tree. Ownership lands on the descriptor
		// (writeOwned fchowns from the handle's fstat), which is what the
		// separate chownLikeStateOwner call used to do by path.
		h, err := openProviderStateDir(*p)
		if err != nil {
			return err
		}
		defer h.Close()
		if err := h.writeOwned(markerName, []byte(state+"\n"), 0o644); err != nil {
			return err
		}
	} else {
		// Legacy path: no target, no provider discovery.
		home := os.Getenv("HOME")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		if home == "" {
			return fmt.Errorf("cannot resolve self-heal marker path: $HOME is not set")
		}
		markerPath := filepath.Join(home, ".urnetwork", markerName)
		if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(markerPath, []byte(state+"\n"), 0o644); err != nil {
			return err
		}
	}
	// When a target was given, the handle handed the file to the provider's
	// user at write time. For the legacy path (no target, no provider
	// discovery) the file stays owned by the caller — that's the pre-H6
	// behavior.
	if state == "on" {
		fmt.Println("self-heal enabled (pressure actuators active; see 'urnet-tools self-heal status')")
	} else {
		fmt.Println("self-heal disabled (pressure actuators turned off)")
	}
	return nil
}

func showSelfHeal(targetArgs []string) error {
	markerPath, err := selfHealPath(targetArgs)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(markerPath)
	switch {
	case err == nil:
		switch strings.TrimSpace(string(b)) {
		case "on":
			fmt.Println("self-heal: on")
		default:
			fmt.Println("self-heal: off")
		}
	case os.IsNotExist(err):
		fmt.Println("self-heal: off (default; enable with 'urnet-tools self-heal on' or URNETWORK_SELF_HEAL=1)")
	default:
		return err
	}
	return nil
}
