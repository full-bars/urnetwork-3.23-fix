package urnettools

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newDirectCmd creates the `urnet-tools direct <on|off>` subcommand.
// It toggles whether the provider provides on its direct/local IP address.
// When the provider is running, it delegates to the provider binary's own
// `direct` subcommand (which writes the toggle file and triggers a reload).
// When the provider is not running, it writes the toggle file directly to
// the provider's state directory so it takes effect on next start.
func newDirectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "direct [on|off]",
		Short: "toggle providing on the machine's direct/local IP",
		Long:  "Turn providing on the machine's direct/local IP address on or off. By default both the direct IP and the configured proxy list are used for providing. Running 'direct off' disables the direct IP — only proxies from --proxy_file / --proxy_url are used. The change takes effect immediately on a running provider (via reload) or on next start. Also settable at startup via DISABLE_DIRECT_IP=1 env var (cannot be toggled at runtime).",
		Example: `  urnet-tools direct off
  urnet-tools direct off --unit urnetwork-native.service
  urnet-tools direct on`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdDirectToggle(rest, force, dryRun)
			})
		},
	}
}

// cmdDirectToggle implements the urnet-tools `direct` subcommand.
func cmdDirectToggle(args []string, force, dryRun bool) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return fmt.Errorf("direct takes on|off only (got %v)", rest)
	}

	switch sub {
	case "on", "off":
		// proceed to write toggle
	case "", "status":
		return cmdDirectStatus(t)
	default:
		return fmt.Errorf("direct action must be on, off, or status (got %q)", sub)
	}

	providers := Discover()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	// The direct-toggle file lives in the provider's ~/.urnetwork/ dir.
	// On the provider binary, that resolves to $HOME/.urnetwork/direct,
	// which maps to StateDir/.urnetwork/direct when the provider runs
	// as a different user (home = parent of state dir).
	home := homeForUser(p.User)
	if home == "" && p.StateDir != "" {
		home = filepath.Dir(p.StateDir)
	}
	togglePath := filepath.Join(home, ".urnetwork", "direct")

	val := "on\n"
	action := "enable"
	if sub == "off" {
		val = "off\n"
		action = "disable"
	}

	if dryRun {
		fmt.Printf("[dry-run] would %s direct/local IP providing for %s (write %s)\n",
			action, providerLabel(p), togglePath)
		return nil
	}

	toggleDir := filepath.Dir(togglePath)
	if err := os.MkdirAll(toggleDir, 0700); err != nil {
		return fmt.Errorf("could not create dir for direct toggle: %v", err)
	}
	// Match every other state-write in this package (session_cmds.go,
	// self_heal.go): a root-run `urnet-tools direct` must not leave a
	// root-owned dir/file the provider's unprivileged user can't read, or
	// the toggle silently falls back to default behavior for that user.
	if err := chownLikeStateOwner(p.StateDir, toggleDir); err != nil {
		return fmt.Errorf("could not set owner on direct toggle dir: %v", err)
	}
	if err := os.WriteFile(togglePath, []byte(val), 0600); err != nil {
		return fmt.Errorf("could not write direct toggle for %s: %v", providerLabel(p), err)
	}
	if err := chownLikeStateOwner(p.StateDir, togglePath); err != nil {
		return fmt.Errorf("could not set owner on direct toggle file: %v", err)
	}

	if p.Running {
		// Trigger a reload so the running provider picks up the change immediately.
		reloadPath := filepath.Join(home, ".urnetwork", "proxy.reload")
		if err := writeReloadTrigger(reloadPath); err != nil {
			fmt.Fprintf(os.Stderr, "[direct] warn: could not signal provider reload: %v\n", err)
		}
	}

	if sub == "off" {
		fmt.Printf("direct: disabled direct/local IP providing for %s\n", providerLabel(p))
	} else {
		fmt.Printf("direct: enabled direct/local IP providing for %s\n", providerLabel(p))
	}
	return nil
}

// cmdDirectStatus shows the current direct-toggle state for the targeted provider.
func cmdDirectStatus(t Target) error {
	providers := Discover()
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	home := homeForUser(p.User)
	if home == "" && p.StateDir != "" {
		home = filepath.Dir(p.StateDir)
	}
	togglePath := filepath.Join(home, ".urnetwork", "direct")
	b, err := os.ReadFile(togglePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("direct: on for %s (default — no override file)\n", providerLabel(p))
			return nil
		}
		return fmt.Errorf("direct: could not read toggle file %s: %v", togglePath, err)
	}
	s := string(b)
	if s == "off\n" || s == "off" {
		fmt.Printf("direct: off for %s (direct/local IP providing disabled)\n", providerLabel(p))
	} else {
		fmt.Printf("direct: on for %s (direct/local IP providing enabled)\n", providerLabel(p))
	}
	return nil
}

// writeReloadTrigger writes (or increments) a reload trigger file at the given path.
// Used by urnet-tools to signal a running provider to hot-reload.
func writeReloadTrigger(path string) error {
	seq := 0
	if b, err := os.ReadFile(path); err == nil {
		fmt.Sscanf(string(b), "%d", &seq)
	}
	seq++
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", seq)), 0600)
}
