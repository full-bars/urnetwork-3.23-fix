package urnettools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The wallet group: help renders from the cobra tree (never by running the
// tool), the fleet-node warning covers the seed-bearing form without blocking it, and the
// delegation passes the provider's own flags through untouched.

func walletTestProvider() Provider {
	return Provider{
		Unit:    "urnetwork-native.service",
		User:    "urnet",
		Binary:  "/usr/bin/provider",
		Running: true,
	}
}

// walletWorkstationProvider is the non-fleet shape: a provider the operator
// runs from their own home, not a systemd service.
func walletWorkstationProvider() Provider {
	return Provider{
		Unit:    "manual",
		User:    "operator",
		Binary:  "/usr/bin/provider",
		Running: true,
	}
}

// withWalletSeams installs the test seams and restores them on cleanup.
func withWalletSeams(t *testing.T, providers []Provider, recorder *[][]string, scriptPath, readmePath string) {
	t.Helper()
	prevProviders := discoverProvidersForWallet
	prevDocker := discoverDockerForWallet
	prevSub := walletProviderSubcommand
	discoverProvidersForWallet = func() []Provider { return providers }
	discoverDockerForWallet = func() []Provider { return nil }
	if recorder != nil {
		walletProviderSubcommand = func(p Provider, args ...string) error {
			*recorder = append(*recorder, append([]string{}, args...))
			return nil
		}
	}
	t.Cleanup(func() {
		discoverProvidersForWallet = prevProviders
		discoverDockerForWallet = prevDocker
		walletProviderSubcommand = prevSub
	})
}

func TestWalletHelpRendersFromTheTree(t *testing.T) {
	if !strings.Contains(rootHelpMenu, "wallet status") {
		t.Fatal("the root help menu must list the wallet group")
	}
	root := buildRootCmd()
	wallet, _, err := root.Find([]string{"wallet"})
	if err != nil || wallet.Name() != "wallet" {
		t.Fatalf("wallet command not found: %v", err)
	}
	if wallet.Short == "" || wallet.Long == "" || wallet.Example == "" {
		t.Fatalf("wallet group must have Short/Long/Example: short=%q long=%q example=%q", wallet.Short, wallet.Long, wallet.Example)
	}
	for _, name := range []string{"status", "challenge", "set", "sign"} {
		var sub *cobra.Command
		for _, candidate := range wallet.Commands() {
			if candidate.Name() == name {
				sub = candidate
			}
		}
		if sub == nil {
			t.Fatalf("wallet %s missing", name)
		}
		if sub.Short == "" || sub.Long == "" || sub.Example == "" {
			t.Fatalf("wallet %s must have Short/Long/Example: short=%q long=%q example=%q", name, sub.Short, sub.Long, sub.Example)
		}
		if !strings.Contains(sub.Example, "urnet-tools wallet") {
			t.Fatalf("wallet %s Example must show real invocations, got %q", name, sub.Example)
		}
	}
	// the seed-file warning is loud in the Long text
	var setCmd *cobra.Command
	for _, candidate := range wallet.Commands() {
		if candidate.Name() == "set" {
			setCmd = candidate
		}
	}
	if !strings.Contains(setCmd.Long, "OWN WORKSTATION") || !strings.Contains(setCmd.Long, "never a fleet node") {
		t.Fatal("wallet set Long must carry the workstation-only warning")
	}
}

func TestWalletStatusDelegates(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	if err := cmdWalletStatus([]string{"--unit", "urnetwork-native.service"}); err != nil {
		t.Fatalf("wallet status: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet status" {
		t.Fatalf("delegation args = %v, want [wallet status]", calls)
	}
}

func TestWalletChallengeRequiresAColdkey(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	err := cmdWalletChallenge(nil)
	if err == nil || !strings.Contains(err.Error(), "coldkey") {
		t.Fatalf("wallet challenge with no coldkey must give a helpful error, got %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("nothing may be delegated without a coldkey")
	}
	// with a coldkey the challenge delegates and passes provider flags through
	err = cmdWalletChallenge([]string{"5CvF...ss58", "--wallet-from-epoch", "51", "-f"})
	if err != nil {
		t.Fatalf("wallet challenge: %v", err)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.HasPrefix(joined, "wallet challenge 5CvF...ss58") || !strings.Contains(joined, "--wallet-from-epoch 51") {
		t.Fatalf("delegation args = %q", joined)
	}
}

// The seed-file form on a fleet node warns and proceeds. No -f and no
// terminal: this is the exact case that used to refuse. The operation is
// asserted by the delegation to the provider, not by the warning text.
func TestWalletSetSeedFileProceedsOnFleetNodeWithoutTerminal(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	withWalletNoTerminal(t)
	warned := withWalletWarnCapture(t)
	err := cmdWalletSet([]string{"5CvF...ss58", "--coldkey_seed_file=/tmp/seed"})
	if err != nil {
		t.Fatalf("the seed-file form on a fleet node must proceed, got %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("the seed-file form must delegate exactly once, got %v", calls)
	}
	if joined := strings.Join(calls[0], " "); joined != "wallet set 5CvF...ss58 --coldkey_seed_file=/tmp/seed" {
		t.Fatalf("delegation args = %q", joined)
	}
	if !strings.Contains(warned.String(), "WARNING") {
		t.Fatalf("a fleet node must get the warning, got %q", warned.String())
	}
	// The removed flag is still rejected by name before anything runs.
	calls = nil
	err = cmdWalletSet([]string{"-f", "--i-am-on-my-own-workstation", "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"})
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("a removed flag must be rejected by name, got %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("a rejected call must not delegate")
	}
	// the message/signature form is untouched by the fleet rule
	calls = nil
	err = cmdWalletSet([]string{"-f", "5CvF...ss58", "--message=m", "--signature=0x00"})
	if err != nil {
		t.Fatalf("wallet set signed form: %v", err)
	}
	if len(calls) != 1 {
		t.Fatal("the signed form must delegate without the fleet gate")
	}
}

// Off a fleet node there is no warning and still no prompt.
func TestWalletSetSeedFileProceedsOnWorkstationWithoutTerminal(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletWorkstationProvider()}, &calls, "", "")
	withWalletNoTerminal(t)
	warned := withWalletWarnCapture(t)
	if err := cmdWalletSet([]string{"5CvF...ss58", "--coldkey_seed_file=/tmp/seed"}); err != nil {
		t.Fatalf("the seed-file form on a workstation must proceed, got %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("must delegate, got %v", calls)
	}
	if warned.Len() != 0 {
		t.Fatalf("a workstation must not get the fleet warning, got %q", warned.String())
	}
}

// --dry-run still stops before the provider: the warning does not bypass it.
func TestWalletSetSeedFileDryRunStillDoesNotDelegate(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	withWalletNoTerminal(t)
	withWalletWarnCapture(t)
	if err := cmdWalletSet([]string{"--dry-run", "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("dry-run must not delegate, got %v", calls)
	}
}

func TestWalletFleetDetectionViaProviderEnv(t *testing.T) {
	var calls [][]string
	// the discovered provider is workstation-shaped (not a unit), so only the
	// provider runtime environment marks this box as a fleet member
	withWalletSeams(t, []Provider{walletWorkstationProvider()}, &calls, "", "")
	t.Setenv("URNETWORK_PROFILE", "lowmem")
	withWalletNoTerminal(t)
	warned := withWalletWarnCapture(t)
	if err := cmdWalletSet([]string{"5CvF...ss58", "--coldkey_seed_file=/tmp/seed"}); err != nil {
		t.Fatalf("the seed-file form must proceed: %v", err)
	}
	if !strings.Contains(warned.String(), "WARNING") {
		t.Fatal("the provider environment marker must count as a fleet node and warn")
	}
	if len(calls) != 1 {
		t.Fatalf("the set must delegate, got %v", calls)
	}
}

// withWalletNoTerminal makes stdin look like no terminal at all, the case a
// systemd unit, cron or an ssh command without -t presents. It sets the
// override directly rather than calling the linux-tagged test helper, so this
// file still builds on FreeBSD and Windows.
func withWalletNoTerminal(t *testing.T) {
	t.Helper()
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return false }
	t.Cleanup(func() { stdinIsInteractiveOverride = old })
}

// withWalletWarnCapture redirects the warning writer and returns the buffer.
func withWalletWarnCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := walletWarnWriter
	walletWarnWriter = &buf
	t.Cleanup(func() { walletWarnWriter = old })
	return &buf
}

// The removed flag is rejected by name, on every subcommand, and never
// reaches the provider.
func TestWalletRemovedWorkstationFlagRejected(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	for _, args := range [][]string{
		{"-f", "--i-am-on-my-own-workstation", "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"},
		{"-f", "5CvF...ss58", "--message=m", "--signature=0x00", "--i-am-on-my-own-workstation"},
	} {
		if err := cmdWalletSet(args); err == nil || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("wallet set must reject the removed flag by name, got %v", err)
		}
	}
	if err := cmdWalletSign([]string{"--i-am-on-my-own-workstation", "5CvF...ss58"}); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatal("wallet sign must reject the removed flag by name")
	}
	if len(calls) != 0 {
		t.Fatalf("a rejected call must not delegate, got %v", calls)
	}
}

// The browser form signs in the extension and never reads a seed, so the
// fleet-node guard must not apply to it even when the box looks like one.
func TestWalletSignNotGatedOnFleetNode(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	t.Setenv("URNETWORK_PROFILE", "lowmem")
	if err := cmdWalletSign([]string{"-f", "5CvF...ss58", "--no-open"}); err != nil {
		t.Fatalf("wallet sign must not be gated on a fleet node: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("wallet sign must delegate on a fleet node, got %v", calls)
	}
}

func TestWalletSignDelegatesToTheProviderSigningPage(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletWorkstationProvider()}, &calls, "", "")
	// -f skips the confirm prompt
	if err := cmdWalletSign([]string{"-f", "5CvF...ss58", "--no-open"}); err != nil {
		t.Fatalf("wallet sign: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet sign 5CvF...ss58 --no-open" {
		t.Fatalf("wallet sign must run 'provider wallet sign' with the arguments, got %v", calls)
	}
	// the address is optional: the account is chosen on the page
	calls = nil
	if err := cmdWalletSign([]string{"-f"}); err != nil {
		t.Fatalf("wallet sign without an address: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet sign" {
		t.Fatalf("wallet sign without an address must still delegate, got %v", calls)
	}
}
