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
		Long:  "Turn providing on the machine's direct/local IP on or off. With 'off' only proxies from --proxy_file / --proxy_url are used. The change applies to a running provider immediately, or on next start, and can also be fixed at startup with DISABLE_DIRECT_IP=1 (which cannot be toggled at runtime).",
		Example: `  urnet-tools direct off
  urnet-tools direct off --unit urnetwork-native.service
  urnet-tools direct on`,
		// Without this, cobra consumes the root persistent flags
		// (--unit/--user/--network/--state-dir/-f/-n) BEFORE the handler
		// runs, so `direct off --unit B` and `direct off -n` silently lose
		// their target and dry-run — wrong-provider or destructive behavior.
		// Every other command routes through newCobraCmd which sets this;
		// direct was the holdout.
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
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
	// Write the toggle through a descriptor-pinned handle (the state dir
	// itself is opened O_NOFOLLOW): the marker and its ownership are set
	// relative to ONE open directory, so a provider user who swaps the
	// state dir for a symlink can no longer redirect the write. Ownership
	// lands on the descriptor (writeOwned fchowns from the handle's
	// fstat), which replaces the two pathname lookups the old code made
	// (chownLikeStateOwner by path plus writeStateFileOwned).
	h, err := openStateDirInCreate(p.StateHome, toggleDir)
	if err != nil {
		return fmt.Errorf("could not create or open direct toggle dir %s: %v", toggleDir, err)
	}
	defer h.Close()
	// The handle takes its owner from toggleDir itself, and a root-run
	// MkdirAll above leaves a NEW dir owned by root (0700), which would lock
	// the provider's unprivileged user out of the toggle. Hand the directory
	// to the state dir's owner first, through the open descriptor.
	ref, err := openProviderStateDir(p)
	if err != nil {
		return fmt.Errorf("could not open state dir %s: %v", p.StateDir, err)
	}
	defer ref.Close()
	if err := h.adoptOwnerOf(ref); err != nil {
		return fmt.Errorf("could not set owner on direct toggle dir: %v", err)
	}
	if err := h.writeOwned("direct", []byte(val), 0o600); err != nil {
		return fmt.Errorf("could not write direct toggle for %s: %v", providerLabel(p), err)
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
	// Read and write through a descriptor-pinned handle on the state dir: the
	// file sits in a user-owned directory and this runs as root, so the read
	// must not follow a planted symlink, the write must not be redirected by a
	// swapped directory, and the file is handed to the directory's owner.
	h, err := openStateDirHandle(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer h.Close()
	name := filepath.Base(path)
	seq := 0
	if b, rerr := h.readFile(name, 64); rerr == nil {
		fmt.Sscanf(string(b), "%d", &seq)
	}
	seq++
	return h.writeOwned(name, []byte(fmt.Sprintf("%d\n", seq)), 0600)
}
