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

const extenderUsage = "usage: urnet-tools extender [status|about|on|off|enable|disable|setup]"
const extenderSetupUsage = "usage: urnet-tools extender setup [-y|--yes] [-f|--force] [-n|--dry-run] [target]"

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

func isExtenderSetupArgs(args []string) bool {
	_, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		for _, a := range args {
			if strings.EqualFold(strings.TrimSpace(a), "setup") {
				return true
			}
		}
		return false
	}
	for _, a := range rest {
		switch a {
		case "-f", "--force", "-y", "--yes", "-n", "--dry-run":
			continue
		default:
			return strings.EqualFold(strings.TrimSpace(a), "setup")
		}
	}
	return false
}

// parseExtenderSetupArgs parses flags specific to `urnet-tools extender setup`.
func parseExtenderSetupArgs(args []string) (yes, dryRun bool, target Target, err error) {
	var filtered []string
	for _, tok := range args {
		switch tok {
		case "-y", "--yes", "-f", "--force":
			yes = true
		case "-n", "--dry-run":
			dryRun = true
		default:
			filtered = append(filtered, tok)
		}
	}
	target, rest, err := parseTargetFlags(filtered)
	if err != nil {
		return false, false, Target{}, err
	}
	seenSetup := false
	for _, tok := range rest {
		if !seenSetup && strings.EqualFold(strings.TrimSpace(tok), "setup") {
			seenSetup = true
			continue
		}
		return false, false, Target{}, fmt.Errorf("%s (unexpected %q)", extenderSetupUsage, tok)
	}
	if !seenSetup {
		return false, false, Target{}, fmt.Errorf("%s", extenderSetupUsage)
	}
	return yes, dryRun, target, nil
}

var runExtenderSetupFn = RunExtenderSetup

func cmdExtenderSetup(args []string) error {
	yes, dryRun, target, err := parseExtenderSetupArgs(args)
	if err != nil {
		return err
	}
	argv := append([]string{"extender"}, args...)
	return runExtenderSetupFn(ExtenderSetupOptions{
		Yes:    yes,
		Force:  yes,
		DryRun: dryRun,
		Target: target,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Argv:   argv,
	})
}

func newExtenderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extender [status|about|on|off|enable|disable|setup]",
		Short: "show, turn on, turn off, or prepare the host for the extender role",
		Long: "The extender helps censored clients reach URnetwork: this host accepts their connections on tcp 443 (required), udp 443 and udp 4053 and relays them, and asks the platform to list it. " +
			"It is ON by default and earns nothing; it is a public service like a Tor bridge. " +
			"'status' shows whether it is working: which ports are up, what IPv4 and IPv6 are doing and when each tries next, who it is helping, and what to fix if it is blocked. " +
			"'about' explains the ports, the firewall, IPv6, Docker and what survives a restart, and works with the provider stopped. " +
			"'setup' inspects port 443 occupancy, binary bind capability and host firewall rules, prints the exact commands needed, and asks for confirmation before changing anything (use -y/--yes for non-interactive runs). UDP 53 is optional and user-managed; setup does not open it. FreeBSD extender setup is untested and prints manual instructions only. " +
			"'off' (or 'disable') turns it off and keeps it off across restarts; 'on' (or 'enable') turns it back on. Both apply to the running provider at once, with no restart and no unit file edit. " +
			"In Docker this command is not available: read `docker logs <container> | grep 🌉` and set URNETWORK_EXTENDER=off to disable it. " +
			"Honors -n/--dry-run, which prints the plan and sends nothing.",
		Example:            "  urnet-tools extender\n  urnet-tools extender status\n  urnet-tools extender about\n  urnet-tools extender setup\n  urnet-tools extender setup --yes\n  urnet-tools extender disable\n  urnet-tools extender enable",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			if isExtenderSetupArgs(args) {
				return cmdExtenderSetup(args)
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				if force {
					return fmt.Errorf("%s (only 'extender setup' accepts -y/--yes or -f/--force)", extenderUsage)
				}
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

	if !dryRun {
		if elevated, err := maybeElevateForCrossUser("extender", p, args, false, dryRun); elevated {
			return err
		}
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
  urnet-tools extender setup    check port 443, binary capability and firewall rules

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
  udp 53    optional and user-managed. Setup does not open UDP 53. It is tried
            automatically; if a resolver holds it, the extender warns and
            keeps using 4053. URNETWORK_EXTENDER_DNS53=off skips it.
  Without tcp 443 the extender opens none of the other ports: the platform
  could not list this host anew, so nothing could use them. (A listing the
  platform made earlier stays valid until it expires.) It retries tcp 443
  every few minutes and opens the rest once it gets it. A hot-swap can race
  the old process releasing the bind; the retry can delay extender recovery.

HOW IT BECOMES ACTIVE
  The provider asks the platform to list this host, once for IPv4 and once
  for IPv6. The platform connects back to check it. A listing has an expiry
  and is renewed. Refusals are paced (the next try time is logged and
  survives a restart). IPv6 only works if this host has a public IPv6 address.
  After a restart a listing that is still valid shows as active at once, and
  the provider refreshes it within about 30 minutes.

FIREWALL
  Opening a port in the provider does not open it on the host. Run
  'urnet-tools extender setup' to inspect the binary capability and host
  firewall, print the exact commands needed, and apply them after confirmation.
  FreeBSD extender setup is untested this release and prints manual commands
  only; it does not elevate or change the host.
  If you prefer to configure the host firewall manually, allow tcp 443
  (required) and optionally udp 443 and udp 4053:
    ufw:       sudo ufw allow 443/tcp   (and 443/udp, 4053/udp)
    firewalld: sudo firewall-cmd --permanent --add-port=443/tcp --add-port=443/udp
               --add-port=4053/udp && sudo firewall-cmd --reload
    iptables:  sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT   (also udp
               443 and 4053; ip6tables for IPv6; save the rules to keep them)
    nftables:  sudo nft add rule inet filter input tcp dport 443 accept
               (adjust the table and chain names to your ruleset)
  A router, or a cloud provider's own firewall (Oracle security list or
  network security group, GCP VPC firewall rule, AWS security group, Azure
  network security group) blocks inbound ports by default and needs the same
  ports allowed too.
  If the log says "left out = dns:4053 ... no answer" or "quic:443 ... no
  answer", those udp ports did not answer from outside; tcp is enough.

IPV6
  IPv6 activates separately and needs a public IPv6 address AND a working
  route out. Check: ip -6 route show default, and curl -6 https://api.bringyour.com/hello
  A host with an address but no route times out after about a minute.

DOCKER
  Use --network host, or publish -p 443:443 -p 443:443/udp and
  -p 4053:4053/udp. The latter does not publish UDP 53; add -p 53:53/udp
  only if you want it and the host port is free. Manage firewall rules yourself.
  For Compose IPv6, enable IPv6 on its network and configure routing and port
  publishing; changing only Docker's default bridge does not configure a
  Compose-created network. Verify reachability from outside.
  Keep /root/.urnetwork on a persistent volume. Read the state with
  docker logs <container> | grep 🌉 and disable with -e URNETWORK_EXTENDER=off.
  Known limitation: the Docker RAM log follower may replay retained lines after
  the log is trimmed.

WHAT IT EXPOSES
  The DNS ports also answer ordinary DNS questions from anyone, resolved
  over DoH and heavily rate limited, so a prober gets a realistic answer.
  If you do not want that, firewall udp 4053 and 53, or turn the role off.

WHERE TO LOOK
  urnet-tools extender status    ports, IPv4 and IPv6, clients now, requests served
  urnet-tools extender setup     prepare host capability and firewall rules
  urnet-tools top                the same, live
  urnet-tools logs               every extender line starts with the bridge emoji
  The health line [health][extender] repeats the summary every few minutes.
  More: docs/Extender.md
`
