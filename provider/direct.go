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

// direct.go implements the operator `provider direct <state>` runtime toggle:
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
		// Distinguish permission errors from other failures: a permission
		// mismatch means the toggle file exists but the provider can't read
		// it (e.g. root-owned file, unprivileged provider). Log it so the
		// operator knows the toggle is silently ignored (finding #7).
		tlog("[direct] warn: could not read toggle file: %v\n", err)
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	val := "on"
	if !enabled {
		val = "off"
	}
	// Atomic write: temp file + rename to avoid a reload reading a
	// partially-written file (which parses as "on" = spurious enable).
	tmp, err := os.CreateTemp(dir, "direct-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp toggle file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(val + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp toggle file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp toggle file: %w", err)
	}
	// TODO: add chownLikeStateOwner(dir, tmpPath) when provider gains access
	// to the urnettools chown utilities — preserves ownership across
	// sudo/cli → provider transitions.
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming toggle file: %w", err)
	}
	return nil
}

// clearDirectToggle removes the control file, restoring the default (on).
func clearDirectToggle() error {
	path, err := directControlPath()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// cmdDirect implements `provider direct <state>`: toggles the runtime
// direct-IP toggle and triggers a reload of a running provider.
func cmdDirect(opts docopt.Opts) {
	arg, _ := opts.String("<state>")
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
		fmt.Fprintf(os.Stderr, "Usage: provider direct <state> (on|off)\n")
		os.Exit(2)
	}

	// If the provider is running, trigger an immediate reload so the
	// change takes effect without a restart. (L6: hold proxyStateMu for
	// the read so this can't interleave with reload()'s state write.)
	proxyStateMu.Lock()
	state, err := readProxyState()
	proxyStateMu.Unlock()
	if err != nil {
		return
	}
	if state != nil && !state.StartedAt.IsZero() {
		triggerProxyReload()
	}
}
