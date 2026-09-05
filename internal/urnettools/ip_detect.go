// ip_detect.go — `urnet-tools ip-detect on|off|status` command
//
// Toggles or reports the provider's IP autodetection behavior. The provider
// fetches its public IPv4 via ip.me at mint/renewal time unless
// URNETWORK_PUBLIC_IP is set or ~/.urnetwork/disable_ip_autodetect exists.
// This command creates/removes that disable file so no restart is needed.

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdIPDetect toggles or reports the provider's IP autodetection marker file.
//
//	urnet-tools ip-detect on       enable (let provider auto-fetch public IP)
//	urnet-tools ip-detect off      disable (provider won't autodetect IP)
//	urnet-tools ip-detect status   report current state
func cmdIPDetect(args []string) error {
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
			return fmt.Errorf("unknown ip-detect sub-arg %q (on|off|status)", args[0])
		}
	}
	switch mode {
	case "on":
		return disableIPDetection(rest, false)
	case "off":
		return disableIPDetection(rest, true)
	case "status":
		return showIPDetection(rest)
	default:
		return fmt.Errorf("unknown ip-detect sub-arg %q (on|off|status)", mode)
	}
}

// ipDetectDisabledPath returns ~/.urnetwork/disable_ip_autodetect.
func ipDetectDisabledPath(targetArgs []string) (string, error) {
	if len(targetArgs) == 0 {
		home := os.Getenv("HOME")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		if home == "" {
			return "", fmt.Errorf("cannot resolve IP detection marker path: $HOME is not set")
		}
		return filepath.Join(home, ".urnetwork", "disable_ip_autodetect"), nil
	}
	t, _, err := parseTargetFlags(targetArgs)
	if err != nil {
		return "", err
	}
	p, err := selectTarget(lifecycleCandidates(t), t)
	if err != nil {
		return "", err
	}
	if p.StateDir == "" {
		return "", fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	return filepath.Join(p.StateDir, "disable_ip_autodetect"), nil
}

func disableIPDetection(targetArgs []string, disable bool) error {
	markerPath, err := ipDetectDisabledPath(targetArgs)
	if err != nil {
		return err
	}
	if disable {
		if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(markerPath, []byte("1\n"), 0o644); err != nil {
			return err
		}
		fmt.Println("ip-detect: off (provider won't auto-detect public IP; set URNETWORK_PUBLIC_IP to override)")
	} else {
		if err := os.Remove(markerPath); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
		fmt.Println("ip-detect: on (provider auto-detects public IP via ip.me)")
	}
	return nil
}

func showIPDetection(targetArgs []string) error {
	markerPath, err := ipDetectDisabledPath(targetArgs)
	if err != nil {
		return err
	}
	_, err = os.Stat(markerPath)
	if os.IsNotExist(err) {
		fmt.Println("ip-detect: on (provider auto-detects public IP via ip.me)")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Println("ip-detect: off (provider won't auto-detect public IP; set URNETWORK_PUBLIC_IP to override)")
	return nil
}

// Provider-level wrapper for the cobra command.
func cmdIPDetectProvider(p Provider, args []string) error {
	return cmdIPDetect(args)
}

var _ = strings.TrimSpace // keep strings import used if logic expands
