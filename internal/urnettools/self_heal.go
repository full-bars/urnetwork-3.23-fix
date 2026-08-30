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

func writeSelfHeal(state string, targetArgs []string) error {
	path, err := selfHealPath(targetArgs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(state+"\n"), 0o644); err != nil {
		return err
	}
	// When a target was given, chown to the provider's user. For the
	// legacy path (no target, no provider discovery) the file stays
	// owned by the caller — that's the pre-H6 behavior.
	if len(targetArgs) > 0 {
		t, _, err := parseTargetFlags(targetArgs)
		if err == nil {
			if p, err := selectTarget(lifecycleCandidates(t), t); err == nil {
				_ = chownLikeStateOwner(p.StateDir, path)
			}
		}
	}
	if state == "on" {
		fmt.Println("self-heal enabled (load gate + auto cleanup active)")
	} else {
		fmt.Println("self-heal disabled (load gate + auto cleanup turned off)")
	}
	return nil
}

func showSelfHeal(targetArgs []string) error {
	path, err := selfHealPath(targetArgs)
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("self-heal: off (default; enable with 'urnet-tools self-heal on' or URNETWORK_SELF_HEAL=1)")
			return nil
		}
		return err
	}
	switch strings.TrimSpace(string(b)) {
	case "on":
		fmt.Println("self-heal: on")
	default:
		fmt.Println("self-heal: off")
	}
	return nil
}
