package urnettools

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The wallet group wraps the provider binary's own wallet subcommands
// (`provider wallet status|challenge|set|sign`) the same way auth/choose-network
// wrap theirs; sign runs the provider's own browser signing page. The consent is one
// signature covering every provider client of the network, current and
// future; a fleet node needs nothing. Signing material (a coldkey seed file)
// never belongs on a fleet node: `wallet set --coldkey_seed_file` asks before
// it touches one, and refuses when nothing can answer. There is no flag to
// skip that; a fleet node is not the place to hold a seed.

// seams for tests
var (
	discoverProvidersForWallet = Discover
	discoverDockerForWallet    = DiscoverDocker
	walletProviderSubcommand   = providerSubcommand
)

// fleetProviderEnvNames are the provider runtime environment markers that
// identify a fleet member even when discovery finds nothing (for example
// inside the provider's own unit context).
var fleetProviderEnvNames = []string{
	"URNETWORK_PROFILE",
	"URNETWORK_RAMLOGS",
	"URNETWORK_HEALTH_INTERVAL",
	"URNETWORK_HOT_RESTART",
	"URNETWORK_SKIP_AUDIT",
	"URNETWORK_AUTH_CODE",
}

// onFleetNode reports whether this box looks like a fleet member rather than
// the operator's own workstation: a systemd-managed provider is installed, a
// docker provider runs, or the provider runtime environment is present.
func onFleetNode() bool {
	for _, p := range discoverProvidersForWallet() {
		switch p.Unit {
		case "", "manual", "state-dir":
		default:
			return true
		}
	}
	if len(discoverDockerForWallet()) > 0 {
		return true
	}
	for _, name := range fleetProviderEnvNames {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// walletSeedFileForm reports whether args carry the provider's seed-file
// flag, which is the form that reads a coldkey seed from disk.
func walletSeedFileForm(args []string) bool {
	for _, a := range args {
		if a == "--coldkey_seed_file" || strings.HasPrefix(a, "--coldkey_seed_file=") {
			return true
		}
	}
	return false
}

// walletRemovedWorkstationFlag is an argument this tool used to accept. It is
// rejected by name so an old script gets a sentence instead of a provider
// parse error, and so nobody thinks it still bypasses anything.
const walletRemovedWorkstationFlag = "--i-am-on-my-own-workstation"

func rejectRemovedWorkstationFlag(args []string) error {
	for _, a := range args {
		if a == walletRemovedWorkstationFlag || strings.HasPrefix(a, walletRemovedWorkstationFlag+"=") {
			return fmt.Errorf("%s is gone: on a box that looks like a fleet node the seed-file form now asks a human at the terminal, and nothing can answer that for them", walletRemovedWorkstationFlag)
		}
	}
	return nil
}

// extractWalletToolFlags removes the tool-side flags (-f/--force) from args,
// returning the value and the rest.
func extractWalletToolFlags(args []string) (force bool, rest []string) {
	for _, a := range args {
		switch {
		case a == "-f" || a == "--force":
			force = true
		default:
			rest = append(rest, a)
		}
	}
	return
}

// refuseSeedSigningOnFleetNode returns the refusal error when signing
// material is about to be used on a fleet node. No flag skips this: a human
// at the machine can say yes, and nothing else can.
func refuseSeedSigningOnFleetNode(what string) error {
	if !onFleetNode() {
		return nil
	}
	// A provider installed on this box does not make it a fleet node: the
	// operator runs one on their own workstation too, so the detection alone
	// is not a reason to refuse. Ask a human once instead.
	if stdinIsInteractive() {
		fmt.Fprintf(os.Stderr, "\n%s touches your coldkey's signing material. This box also runs a provider, which normally never holds one.\n", what)
		line, err := confirmStdinRead("Continue on this machine? [y/N] ")
		if err == nil {
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y", "yes":
				return nil
			}
		}
		return fmt.Errorf("cancelled: %s did not run", what)
	}
	return fmt.Errorf(`%s touches your coldkey's signing material and is for your OWN workstation, never a fleet node. This box looks like a fleet member (a provider service or provider environment is present), and a fleet node needs nothing here. Run it from a terminal on your own machine, where it will ask you to confirm`, what)
}

func newWalletCmd() *cobra.Command {
	walletCmd := &cobra.Command{
		Use:     "wallet",
		Short:   "network wallet consent",
		Long:    "Manage the network wallet consent: the coldkey release settlement pays. 'status' shows the stored consent, apart from any legacy login-proof wallet; 'challenge' fetches the exact consent statement to sign (signs nothing); 'set' submits a signature, or signs with a local coldkey seed file; 'sign' runs the browser signing helper, so the seed phrase never leaves your wallet extension. The consent is one signature covering every provider client of your network, current and future. A fleet node needs nothing: run the signing forms on your own workstation only.",
		Example: "  urnet-tools wallet status\n  urnet-tools wallet challenge 5CvF...ss58\n  urnet-tools wallet sign 5CvF...ss58",
	}
	walletCmd.AddCommand(
		newWalletSubCmd(
			"status",
			"show the network wallet consent",
			"Show the network wallet consent stored for this provider's network: the consent coldkey, its epoch interval, and whether it is in effect now, still starts in an epoch, or ended. A legacy login-proof wallet (if any) is listed apart: it is not a consent and release settlement does not pay it. Read-only: delegates to the provider binary and sends nothing that changes state.",
			"  urnet-tools wallet status\n  urnet-tools wallet status --unit urnetwork-native.service",
			cmdWalletStatus,
		),
		newWalletSubCmd(
			"challenge <coldkey_ss58>",
			"fetch the consent statement to sign",
			"Fetch a fresh, verified consent statement for a coldkey from the platform. It signs nothing and changes nothing: it prints the exact message to sign and the epoch interval it covers (default: the next epoch through the full span), for the browser helper or any wallet that signs raw messages. Use --wallet-from-epoch/--wallet-through-epoch to pick an explicit interval; a mistaken interval cannot be undone.",
			"  urnet-tools wallet challenge 5CvF...ss58\n  urnet-tools wallet challenge 5CvF...ss58 --wallet-from-epoch 51 --wallet-through-epoch 65585",
			cmdWalletChallenge,
		),
		newWalletSubCmd(
			"set <coldkey_ss58>",
			"submit a signed consent (or legacy set)",
			"Submit a signed network wallet consent for a coldkey: pass the message from 'wallet challenge' and its signature (--message with --signature), or sign with a local coldkey seed file (--coldkey_seed_file), which is FOR THE OPERATOR'S OWN WORKSTATION ONLY and never a fleet node: on a fleet member the tool asks before it uses one and refuses when nothing can answer. Without a proof this sends the legacy unsigned request, which the platform may refuse under its signed wallet rules. A later consent must start at a later epoch than the one before it.",
			"  urnet-tools wallet sign 5CvF...ss58                 # browser signing, seed stays in the extension\n  urnet-tools wallet set 5CvF...ss58 --message=<text> --signature=0x<128 hex>\n  urnet-tools wallet set 5CvF...ss58 --coldkey_seed_file=~/coldkey  # OWN workstation only",
			cmdWalletSet,
		),
		newWalletSubCmd(
			"sign [coldkey_ss58]",
			"sign the consent in the browser",
			"Serve one local page (127.0.0.1 only, 15 minutes, visible countdowns) where a wallet extension (Talisman, SubWallet, Polkadot{.js}, Nova, or any Polkadot signRaw extension) signs the consent statement, then submit the signature and verify the platform's receipt. Without an address you choose the account on the page. The seed phrase never leaves the extension, so it runs anywhere a browser can reach, including a box that also runs a provider. Needs a browser with the extension on this machine; no extra tools.",
			"  urnet-tools wallet sign 5CvF...ss58\n  urnet-tools wallet sign                       # choose the account on the page\n  urnet-tools wallet sign 5CvF...ss58 --no-open",
			cmdWalletSign,
		),
	)
	return walletCmd
}

func newWalletSubCmd(use, short, long, example string, handler func([]string) error) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		Long:               long,
		Example:            example,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return handler(args)
		},
	}
}

// walletTarget resolves the targeted provider for a wallet subcommand.
func walletTarget(args []string) (Provider, []string, error) {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return Provider{}, nil, err
	}
	p, err := selectTarget(discoverProvidersForWallet(), t)
	if err != nil {
		return Provider{}, nil, err
	}
	return p, rest, nil
}

func cmdWalletStatus(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools wallet status — show the network wallet consent

Usage: urnet-tools wallet status [target]

Shows the network wallet consent stored for this provider's network: the
consent coldkey, its epoch interval, and whether it is in effect. A legacy
login-proof wallet is listed apart; it is not a consent. Read-only:
delegates to the provider binary ('provider wallet status') and changes
nothing. See 'providers' for each provider's --unit/--user/--network.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	p, rest, err := walletTarget(args)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("[dry-run] would delegate 'provider wallet status' on %s\n", providerLabel(p))
		return nil
	}
	return walletProviderSubcommand(p, append([]string{"wallet", "status"}, rest...)...)
}

func cmdWalletChallenge(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools wallet challenge — fetch the consent statement to sign

Usage: urnet-tools wallet challenge <coldkey_ss58> [--wallet-from-epoch N --wallet-through-epoch N] [target]

Fetches a fresh, verified consent statement for the coldkey. It signs
nothing and changes nothing: it prints the exact message and the epoch
interval it covers, for the browser helper (urnet-tools wallet sign) or any
wallet that signs raw messages. Provider flags pass through. See
'providers' for each provider's --unit/--user/--network.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	p, rest, err := walletTarget(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("wallet challenge requires a coldkey ss58 address: urnet-tools wallet challenge <coldkey_ss58>")
	}
	if dryRun {
		fmt.Printf("[dry-run] would delegate 'provider wallet challenge' on %s\n", providerLabel(p))
		return nil
	}
	return walletProviderSubcommand(p, append([]string{"wallet", "challenge"}, rest...)...)
}

func cmdWalletSet(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools wallet set — submit a signed network wallet consent

Usage: urnet-tools wallet set <coldkey_ss58> [--message=<text> --signature=0x<hex> | --coldkey_seed_file=<path>] [--wallet-from-epoch N --wallet-through-epoch N] [target]

Submits the consent for the coldkey. Recommended: sign with 'urnet-tools
wallet sign' (browser, seed stays in the extension) and let it call this.
The --coldkey_seed_file form reads a seed from disk and is FOR THE
OPERATOR'S OWN WORKSTATION ONLY, never a fleet node; on a fleet member the
tool asks before it uses one and refuses when nothing can answer. Without a
proof this sends the legacy unsigned request. Provider flags pass through.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	if err := rejectRemovedWorkstationFlag(args); err != nil {
		return err
	}
	force, args := extractWalletToolFlags(args)
	p, rest, err := walletTarget(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("wallet set requires a coldkey ss58 address: urnet-tools wallet set <coldkey_ss58> [--message --signature | --coldkey_seed_file]")
	}
	if walletSeedFileForm(rest) {
		if err := refuseSeedSigningOnFleetNode("urnet-tools wallet set --coldkey_seed_file"); err != nil {
			return err
		}
	}
	ok, err := confirmGate("set the network wallet consent on "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return walletProviderSubcommand(p, append([]string{"wallet", "set"}, rest...)...)
}

func cmdWalletSign(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools wallet sign — sign the consent in the browser

Usage: urnet-tools wallet sign [coldkey_ss58] [--wallet-from-epoch N --wallet-through-epoch N] [--port N] [--no-open] [target]

Runs 'provider wallet sign': one local page on 127.0.0.1 (15 minutes, with
visible countdowns, extendable from the page) where a wallet extension
(Talisman, SubWallet, Polkadot{.js}, Nova, or any Polkadot signRaw extension)
signs the consent statement. Without an address you choose the account on
the page. The platform's receipt is verified and the result shown on the page
and in the terminal. The seed phrase never leaves the extension, so this runs
anywhere a browser can reach, including a box that also runs a provider.
`)
		return nil
	}
	dryRun, args := stripDryRunFlag(args)
	if err := rejectRemovedWorkstationFlag(args); err != nil {
		return err
	}
	force, args := extractWalletToolFlags(args)
	// The browser form never touches the seed: the extension signs and the
	// seed stays where it lives, so there is nothing here to guard.
	p, rest, err := walletTarget(args)
	if err != nil {
		return err
	}
	ok, err := confirmGate("sign the network wallet consent in the browser with "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return walletProviderSubcommand(p, append([]string{"wallet", "sign"}, rest...)...)
}
