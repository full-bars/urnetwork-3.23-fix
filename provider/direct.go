package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docopt/docopt-go"
)

// directProxyKey is the special cancel-map key under which the native [direct]
// transport goroutine is registered, so reload() can hot-toggle it.
const directProxyKey = "direct"

// direct.go implements the operator `provider direct <on|off>` runtime toggle:
// controls whether the provider launches a direct (local-IP) transport in
// addition to proxies from --proxy_file / --proxy_url. Persisted so it survives
// restarts and reloads. Read by provide() at startup and by reload() to
// start/stop the direct goroutine on the fly.

// directControlPath returns the operator direct-toggle file path.
func directControlPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "direct"), nil
}

// readDirectOverride reads the runtime direct toggle file and returns
// (desiredState, fileExists). When the file does NOT exist (fileExists=false),
// the caller falls back to the DISABLE_DIRECT_IP env var or the default (on).
// When the file exists, its value takes precedence over the env var — this lets
// `provider direct on` re-enable direct even if DISABLE_DIRECT_IP=1 was set,
// and vice versa. The file content is parsed like isDirectEnabled: "off"/"0"/
// "false"/"no" → false, anything else → true.
func readDirectOverride() (bool, bool) {
	path, err := directControlPath()
	if err != nil {
		return true, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, false
		}
		tlog("[direct] warning: could not read toggle file %s (treating as default on): %v\n", path, err)
		return true, false
	}
	s := strings.ToLower(strings.TrimSpace(string(b)))
	if s == "off" || s == "0" || s == "false" || s == "no" {
		return false, true
	}
	return true, true
}

// isDirectEnabled returns true if direct providing should be active at startup.
// Precedence: runtime toggle file (if present) > DISABLE_DIRECT_IP env var >
// default (on). This is the startup-time entry point used by provide().
func isDirectEnabled() bool {
	if directOn, fileExists := readDirectOverride(); fileExists {
		return directOn
	}
	return os.Getenv("DISABLE_DIRECT_IP") != "1"
}

// writeDirectEnabled writes the direct toggle state.
func writeDirectEnabled(enabled bool) error {
	path, err := directControlPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	val := "on"
	if !enabled {
		val = "off"
	}
	return os.WriteFile(path, []byte(val+"\n"), 0o600)
}

// clearDirectToggle removes the control file, restoring the default (on).
func clearDirectToggle() error {
	path, err := directControlPath()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// cmdDirect implements `provider direct <on|off>`: toggles the runtime
// direct-IP toggle and triggers a reload of a running provider.
func cmdDirect(opts docopt.Opts) {
	arg, _ := opts.String("<on|off>")
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on":
		if err := writeDirectEnabled(true); err != nil {
			shmLogFatal(72, "could not enable direct providing: %v", err)
		}
		fmt.Println("direct: providing on direct/local IP is now ENABLED")
	case "off":
		if err := writeDirectEnabled(false); err != nil {
			shmLogFatal(72, "could not disable direct providing: %v", err)
		}
		fmt.Println("direct: providing on direct/local IP is now DISABLED (proxies only)")
	default:
		fmt.Fprintf(os.Stderr, "Usage: provider direct <on|off>\n")
		os.Exit(2)
	}

	// If the provider is running, trigger an immediate reload so the
	// change takes effect without a restart.
	state, err := readProxyState()
	if err != nil {
		return
	}
	if state != nil && !state.StartedAt.IsZero() {
		triggerProxyReload()
	}
}
