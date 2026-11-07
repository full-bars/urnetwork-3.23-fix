package urnettools

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const extenderUsage = "usage: urnet-tools extender [status|on|off|enable|disable]"

// normalizeExtenderAction accepts enable and disable as plain names for on and
// off. The second result is false for anything else.
func normalizeExtenderAction(action string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "status":
		return "status", true
	case "on", "enable":
		return "on", true
	case "off", "disable":
		return "off", true
	}
	return "", false
}

func newExtenderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extender [status|on|off|enable|disable]",
		Short: "show, turn on, or turn off the extender role",
		Long: "The extender helps censored clients reach URnetwork: this host listens on tcp 443, udp 443 and dns 4053, and asks the platform to list it. " +
			"It is ON by default. 'status' shows whether it is working: which ports are up, what each address family is doing, who it is helping, and what to fix if it is blocked. " +
			"'off' (or 'disable') turns it off and keeps it off across restarts; 'on' (or 'enable') turns it back on. Both apply to the running provider at once, with no restart and no unit file edit. " +
			"Honors -n/--dry-run, which prints the plan and sends nothing.",
		Example:            "  urnet-tools extender\n  urnet-tools extender status\n  urnet-tools extender disable\n  urnet-tools extender enable",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdExtender(rest, dryRun)
			})
		},
	}
}

func cmdExtender(args []string, dryRun bool) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	action := "status"
	if len(rest) > 0 {
		var ok bool
		if action, ok = normalizeExtenderAction(rest[0]); !ok {
			return fmt.Errorf("%s (got %q)", extenderUsage, strings.Join(rest, " "))
		}
	}
	if len(rest) > 1 {
		return fmt.Errorf("%s (got %q)", extenderUsage, strings.Join(rest, " "))
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	if action != "status" {
		// `off` is a real persisted value, never a clear: the default is on
		// (see treatsOffAsClear).
		if err := applySetOverride(p, "extender", action, dryRun); err != nil {
			return err
		}
		if dryRun {
			return nil
		}
		// the provider starts or stops the role asynchronously
		time.Sleep(1500 * time.Millisecond)
	}
	return printExtenderStatus(p)
}

// printExtenderStatus prints the block, and returns an error when the provider
// could not answer so scripts see a non-zero exit.
func printExtenderStatus(p Provider) error {
	reading, err := socketExtenderFn(p)
	if err != nil {
		fmt.Fprintf(os.Stdout, "\nExtender: unavailable: %v\n", err)
		return err
	}
	formatExtenderBlock(os.Stdout, *reading, time.Now())
	return nil
}
