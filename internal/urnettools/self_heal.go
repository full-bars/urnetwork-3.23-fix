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
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "on", "off":
		return writeSelfHeal(mode)
	case "status":
		return showSelfHeal()
	default:
		return fmt.Errorf("unknown self-heal sub-arg %q (on|off|status)", mode)
	}
}

// selfHealMarkerPath returns ~/.urnetwork/proxy_self_heal (the same file the
// provider reads to decide whether the self-heal gate is on).
func selfHealMarkerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_self_heal"), nil
}

func writeSelfHeal(state string) error {
	path, err := selfHealMarkerPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(state+"\n"), 0o600); err != nil {
		return err
	}
	if state == "on" {
		fmt.Println("self-heal enabled (load gate + auto cleanup active)")
	} else {
		fmt.Println("self-heal disabled (load gate + auto cleanup turned off)")
	}
	return nil
}

func showSelfHeal() error {
	path, err := selfHealMarkerPath()
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
