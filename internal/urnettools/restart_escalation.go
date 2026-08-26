package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// The update flow used to die on boxes where restarting the provider unit
// needs elevated privileges (polkit "Interactive authentication required").
// Worse, the running tool binary always predates its own restart-flow fixes:
// an update FROM a release with an older restart order executed the OLD
// logic, failed, and reported "1 of 1 provider(s) failed" even though the
// new binary was already swapped in — leaving the operator to run a separate
// manual restart, which is exactly the friction `update -f` exists to avoid.
//
// restartProviderWithFallback closes that gap with a three-step ladder:
//
//  1. restartProvider — the normal smart path (correct manager for the unit,
//     PID signal fallback).
//  2. On an AUTH-CLASS failure (polkit/sudo denial) with a staged tool
//     binary available: `sudo -n <staged-tool> __do-restart …`. Passwordless
//     sudo is detected with `sudo -n true` first — never a surprise password
//     prompt mid-update — and the RETRY RUNS THE NEW BINARY, so restart-flow
//     fixes shipped in this very release are live during this very update.
//  3. Otherwise: print actionable guidance (a scoped one-time polkit rule so
//     future `update -f` runs need nothing extra, plus the immediate manual
//     restart command) instead of a bare errno.

// isAuthRestartFailure reports whether err looks like systemd/polkit refusing
// the restart for lack of privileges ("Interactive authentication required",
// "Access denied", ...). Those are the cases where escalation or guidance
// helps; anything else (no such unit, timeout, ...) propagates unchanged.
// The substring match is deliberately broad: the input here is always
// restartProvider's own systemctl error text, so a rare false positive only
// routes to the harmless guidance path (Sonnet review LOW).
func isAuthRestartFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"Interactive authentication required",
		"Authentication is required",
		"access denied",
		"Access denied",
		"Not authorized",
		"authentication needed",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// sudoAvailableFn reports whether passwordless sudo works for this user
// (`sudo -n true`). Var seam for tests.
var sudoAvailableFn = func() bool {
	out, err := exec.Command("sudo", "-n", "true").CombinedOutput()
	if err != nil {
		_ = out
		return false
	}
	return true
}

// runStagedRestartFn executes `sudo -n <tool> __do-restart --unit U
// [--user X]` using the freshly staged binary. Var seam for tests.
var runStagedRestartFn = func(stagedTool string, p Provider) error {
	if runtime.GOOS != "linux" || p.Unit == "" {
		return fmt.Errorf("staged restart not applicable")
	}
	args := []string{"sudo", "-n", stagedTool, "__do-restart", "--unit", p.Unit}
	if p.User != "" {
		args = append(args, "--user", p.User)
	}
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s restart via staged tool: %v (%s)", programName(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restartFn is the primary strategy, behind a seam so tests can feed the
// ladder crafted failures. Production value: restartProvider.
var restartFn = restartProvider

// restartForUpdate is the hook updateProvider calls for its final restart.
// Default: plain restartProvider. The update flow temporarily swaps this to
// route through restartLadder with the staged tool binary
// (updateProviderWithRestart). A var (not a param) keeps updateProvider's
// signature and every existing caller/test unchanged.
var restartForUpdate = restartProvider

// restartLadder runs the ladder described above. stagedTool is the path of
// THIS release's verified tool binary ("" when unavailable).
func restartLadder(p Provider, stagedTool string) error {
	err := restartFn(p)
	if err == nil {
		return nil
	}
	if !isAuthRestartFailure(err) || stagedTool == "" || runtime.GOOS != "linux" || p.Unit == "" {
		return err
	}
	fmt.Println("restart needs elevated privileges — trying the freshly staged tool binary via passwordless sudo...")
	if !sudoAvailableFn() {
		printRestartElevationGuidance(p)
		return fmt.Errorf("could not restart %s: %v — see the commands above to finish the restart", providerLabel(p), err)
	}
	if rerr := runStagedRestartFn(stagedTool, p); rerr != nil {
		fmt.Printf("note: %v\n", rerr)
		printRestartElevationGuidance(p)
		return fmt.Errorf("could not restart %s: %v — see the commands above to finish the restart", providerLabel(p), err)
	}
	fmt.Printf("restarted %s (via staged tool + sudo)\n", providerLabel(p))
	return nil
}

// printRestartElevationGuidance gives the operator both forward paths: a
// one-time scoped polkit rule (after which plain `update -f` restarts on its
// own, no sudo involved) and the immediate manual restart.
func printRestartElevationGuidance(p Provider) {
	unit := p.Unit
	user := p.User
	if user == "" {
		user = currentUserName()
	}
	var b strings.Builder
	b.WriteString("\nThe provider binary was updated, but restarting " + unit + " needs elevated privileges.\n")
	b.WriteString("\nOne-time permanent fix (grants ONLY this unit restart to ONLY user " + user + ",\nso future 'urnet-tools update -f' runs need nothing extra):\n\n")
	b.WriteString("  sudo tee /etc/polkit-1/rules.d/50-urnetwork-restart.rules >/dev/null <<'EOF'\n")
	b.WriteString("polkit.addRule(function(action, subject) {\n")
	b.WriteString("  if (action.id == \"org.freedesktop.systemd1.manage-units\" &&\n")
	b.WriteString("      action.lookup(\"unit\") == \"" + unit + "\" &&\n")
	b.WriteString("      subject.user == \"" + user + "\")\n")
	b.WriteString("    return polkit.Result.YES;\n")
	b.WriteString("});\n")
	b.WriteString("EOF\n")
	b.WriteString("\nOr restart right now with:\n\n  sudo systemctl restart " + unit + "\n\n")
	fmt.Print(b.String())
}

// stageToolForEscalation downloads + verifies THIS release's tool binary
// into the update staging dir so the restart ladder can execute it via
// passwordless sudo. The file is explicitly chmod'd executable:
// downloadFile writes 0644, and execve fails with EACCES on a file with no
// execute bits even for root (Sonnet review HIGH — without this the entire
// escalation leg could never fire in production). Best effort: returns ""
// when the release has no tool asset or download/verify/chmod fails; the
// ladder then skips escalation and prints guidance.
func stageToolForEscalation(cfg updateConfig) string {
	if cfg.ToolDigest == "" || cfg.ToolAsset == "" || cfg.StageDir == "" {
		return ""
	}
	sp := filepath.Join(cfg.StageDir, cfg.ToolAsset)
	url := cfg.ToolAssetURL
	if url == "" {
		url = toolAssetURL(cfg.Tag, cfg.ToolAsset)
	}
	fmt.Printf("downloading %s\n", url)
	if err := downloadFile(url, sp); err != nil {
		fmt.Printf("note: staged-restart escalation unavailable (%v)\n", err)
		return ""
	}
	if err := verifySHA256(sp, cfg.ToolDigest); err != nil {
		fmt.Printf("note: staged-restart escalation unavailable (%v)\n", err)
		return ""
	}
	if err := os.Chmod(sp, 0o755); err != nil {
		fmt.Printf("note: staged-restart escalation unavailable (chmod: %v)\n", err)
		return ""
	}
	return sp
}

// discoverForRestart is the seam __do-restart validates targets against.
var discoverForRestart = Discover

// newDoRestartCmd registers the HIDDEN internal entry point the updater's
// escalated retry invokes (`<tool> __do-restart --unit U [--user X]`). It
// performs exactly one unitCommand restart with the CALLER's own privileges
// — it confers nothing by itself; the privilege comes from the sudo that
// invoked it. Two scoping guards keep even a narrowly-scoped sudoers grant
// from becoming a generic "restart any unit as root" primitive: the docker
// namespace guard, and a requirement that the unit matches a provider
// Discover() actually sees (Sonnet review MEDIUM).
func newDoRestartCmd() *cobra.Command {
	var unit, user string
	cmd := &cobra.Command{
		Use:    "__do-restart",
		Short:  "internal: restart a provider unit (used by the updater's escalation path)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if unit == "" {
				return fmt.Errorf("__do-restart requires --unit")
			}
			// Namespace guard first: a docker:-prefixed target gets the
			// actionable wrong-tool refusal regardless of discovery state
			// (the FLAGS are checked here — discovery may not even see
			// containers).
			if err := guardSystemdProvider(Provider{Unit: unit, User: user}); err != nil {
				return err
			}
			// Discovery is AUTHORITATIVE: flags may select among discovered
			// records, never override them. A --user that disagrees with the
			// discovered record would otherwise route the restart into a
			// different account's session via machined (-M user@) while
			// passing a unit-name-only check (Sonnet review round 2). When
			// more than one record matches (duplicate unit names across
			// accounts), selection is REFUSED rather than picking a winner
			// — this command is the last line of defense behind scoped
			// sudoers grants, so ambiguity must be explicit (round 3).
			p := Provider{Unit: unit}
			found := false
			matches := 0
			for _, dp := range discoverForRestart() {
				if dp.Unit != unit {
					continue
				}
				if user != "" && dp.User != user {
					continue
				}
				matches++
				if matches == 1 {
					p = dp
					found = true
				}
			}
			if matches > 1 {
				return fmt.Errorf("__do-restart: %d discovered providers match unit %q — refusing ambiguous selection (specify the exact target)", matches, unit)
			}
			if !found {
				return fmt.Errorf("__do-restart: no discovered provider with unit %q (and matching user) — refusing", unit)
			}
			return unitCommand(p, "restart")
		},
	}
	cmd.Flags().StringVar(&unit, "unit", "", "systemd unit to restart")
	cmd.Flags().StringVar(&user, "user", "", "owning OS user (for user-manager units)")
	return cmd
}
