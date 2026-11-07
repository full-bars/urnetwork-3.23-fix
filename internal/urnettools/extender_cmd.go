package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const extenderUsage = "usage: urnet-tools extender [status|about|on|off|enable|disable]"

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
		Use:   "extender [status|about|on|off|enable|disable]",
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
	if len(rest) == 1 {
		switch strings.ToLower(strings.TrimSpace(rest[0])) {
		case "about", "help", "explain":
			// static text: works with the provider stopped
			fmt.Print(extenderAboutText)
			return nil
		}
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
		return finishExtenderChange(os.Stdout, p, action)
	}
	return printExtenderStatus(p)
}

// finishExtenderChange follows a change with the live status. With no provider
// running the change was queued, which is a success: it says so and does not
// ask for a status that cannot exist. A provider that is up but does not answer
// is a real problem and still returns its error.
func finishExtenderChange(out io.Writer, p Provider, action string) error {
	reading, err := socketExtenderFn(p)
	if errors.Is(err, errExtenderProviderNotRunning) {
		fmt.Fprintf(out, "\nThe provider is not running, so the change is saved and applies when the provider starts (extender %s).\n", action)
		return nil
	}
	if err != nil {
		fmt.Fprintf(out, "\nExtender: unavailable: %v\n", err)
		return err
	}
	formatExtenderBlock(out, *reading, time.Now())
	return nil
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

// extenderAboutText is what `urnet-tools extender about` prints. It is plain
// text, needs no provider, and should be kept in step with docs/Extender.md.
const extenderAboutText = `The extender helps clients in censored networks reach URnetwork.
This host accepts their connections and relays them to the platform, so
they can bootstrap when the platform's own addresses are blocked.
It earns nothing; it is a public service, like a Tor bridge.

It is ON by default. Nothing to configure. To change it:
  urnet-tools extender off      turn it off, and keep it off across restarts
  urnet-tools extender on       turn it back on
  urnet-tools extender status   what it is doing right now

PORTS
  tcp 443   REQUIRED. The platform proves this host is an extender by
            connecting back on tcp 443. If something else holds the port
            (a web server such as caddy or nginx), the host cannot be newly
            listed and gets no clients once an earlier listing expires. The
            log says when it retries, and names what holds the port when the
            host will tell it.
            A non-root provider also needs permission to use a port below
            1024: sudo setcap cap_net_bind_service=+ep <provider binary>
  udp 443   optional. QUIC carrier.
  udp 4053  optional. DNS carrier.
  udp 53    optional. A second DNS port, tried automatically. If another
            resolver already holds it, the extender logs one warning and
            keeps using 4053 (it may repeat the warning if the role restarts).
            URNETWORK_EXTENDER_DNS53=off skips it.
  Without tcp 443 the extender opens none of the other ports: the platform
  could not list this host anew, so nothing could use them. (A listing the
  platform made earlier stays valid until it expires.) It retries tcp 443
  every few minutes and opens the rest once it gets it.

HOW IT BECOMES ACTIVE
  The provider asks the platform to list this host, once for IPv4 and once
  for IPv6. The platform connects back to check it. A listing has an expiry
  and is renewed. Refusals are paced (the next try time is logged and
  survives a restart). IPv6 only works if this host has a public IPv6 address.
  After a restart a listing that is still valid shows as active at once, and
  the provider refreshes it within about 30 minutes.

WHAT IT EXPOSES
  The DNS ports also answer ordinary DNS questions from anyone, resolved
  over DoH and heavily rate limited, so a prober gets a realistic answer.
  If you do not want that, firewall udp 4053 and 53, or turn the role off.

WHERE TO LOOK
  urnet-tools extender status    ports, IPv4 and IPv6, clients now, requests served
  urnet-tools top                the same, live
  urnet-tools logs               every extender line starts with the bridge emoji
  The health line [health][extender] repeats the summary every few minutes.
  More: docs/Extender.md
`
