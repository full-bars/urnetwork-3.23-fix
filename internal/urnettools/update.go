package urnettools

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// updateConfig holds the release metadata for the update command.
type updateConfig struct {
	// Tag is the release tag to install, e.g. "v3.23.0-fix.26.8".
	Tag string
	// Digest is the sha256 of the release tarball asset (hex). When empty,
	// integrity verification is skipped (not recommended).
	Digest string
	// AssetURL is the download URL for the tarball.
	AssetURL string
	// StageDir is where downloads/extraction happen. MUST be on real disk —
	// /tmp is frequently a small tmpfs and the multi-platform tarball
	// overflows it (the 2026-08-09 failure).
	StageDir string
	// ToolAsset is the release asset name of THIS tool binary
	// (urnet-tools-<os>-<arch> or urnet-docker-<os>-<arch>). Populated by
	// cmdUpdate from the release's asset list.
	ToolAsset string
	// ToolDigest is the sha256 (hex) of the ToolAsset, resolved from the
	// release API. Empty means the release predates tool assets — the
	// self-update leg then skips rather than unverified-downloads.
	ToolDigest string
	// ToolAssetURL is the download URL for the tool's own asset. Distinct
	// from AssetURL (the provider tarball): the self-update leg must NEVER
	// reuse AssetURL, which is provider-scoped.
	ToolAssetURL string
}

// newStageDir creates a private 0700 staging directory for one update.
// Stage on real disk, NOT /tmp (frequently a small tmpfs that the
// multi-platform tarball overflows — the 2026-08-09 failure). Windows has
// no /var/tmp; use the system temp dir there. A
// predictable path could be pre-created by a local user who then swaps the
// tarball between verify and extract.
func newStageDir() (string, error) {
	parent := "/var/tmp"
	if runtime.GOOS == "windows" {
		parent = os.TempDir()
	}
	stageDir, err := os.MkdirTemp(parent, "urnet-stage-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	return stageDir, nil
}

// cmdUpdate updates one or more providers' binaries, then restarts the unit
// that actually owns each (system-level or user-level — never the wrong
// one). Destructive gate applies per provider.
//
// Interactive-first: with no --tag it fetches the latest release from
// GitHub and prompts; with multiple providers and no target it shows the
// numbered picker. Non-interactive (no TTY) refuses ambiguity unless an
// explicit target or --include is given — scripts must be explicit.
func cmdUpdate(args []string, force, dryRun bool) error {
	// LENIENT target parse: update defines its own flags (--tag, --digest,
	// --url, --include, --exclude, --all, --select) which the loop below
	// consumes. Strict parsing here would reject them as unknown before
	// the loop ever runs. Leftover
	// unknown --flags are rejected AFTER the loop instead.
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	cfg := updateConfig{}
	// Parse --tag/--digest/--url and batch-selection overrides.
	var include, exclude []string
	all := false
	interactive := forceInteractive(force) // -f implies non-interactive: no pickers
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--tag":
			if i+1 >= len(rest) {
				return fmt.Errorf("--tag requires a value")
			}
			cfg.Tag = rest[i+1]
			i++
		case "--digest":
			if i+1 >= len(rest) {
				return fmt.Errorf("--digest requires a value")
			}
			cfg.Digest = rest[i+1]
			i++
		case "--url":
			if i+1 >= len(rest) {
				return fmt.Errorf("--url requires a value")
			}
			cfg.AssetURL = rest[i+1]
			i++
		case "--include":
			if i+1 >= len(rest) {
				return fmt.Errorf("--include requires a value (comma-separated labels)")
			}
			include = splitLabels(rest[i+1])
			i++
		case "--exclude":
			if i+1 >= len(rest) {
				return fmt.Errorf("--exclude requires a value (comma-separated labels)")
			}
			exclude = splitLabels(rest[i+1])
			i++
		case "--all", "-all":
			all = true
		case "--select":
			interactive = !force // --select forces the picker unless -f
		default:
			// Accept the = form (--include=a,b) as well as the space form.
			if strings.HasPrefix(rest[i], "--include=") {
				include = splitLabels(strings.TrimPrefix(rest[i], "--include="))
			} else if strings.HasPrefix(rest[i], "--exclude=") {
				exclude = splitLabels(strings.TrimPrefix(rest[i], "--exclude="))
			} else if strings.HasPrefix(rest[i], "-") {
				// Unknown --flag (typo like --netwrok): reject AFTER the
				// command's own flags were consumed.
				return fmt.Errorf("unknown flag %q for update (--tag/--digest/--url/--include/--exclude/--all/--select; targeting via --unit/--user/--network/--network-id/--state-dir)", rest[i])
			} else {
				return fmt.Errorf("unexpected argument %q for update", rest[i])
			}
		}
	}

	providers := Discover()
	var chosen []Provider
	if all {
		// --all means every provider on the box, no ambiguity. It conflicts
		// with an explicit target — error rather than silently discarding
		// it.
		if t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != "" {
			return fmt.Errorf("--all conflicts with an explicit target (%s); use one or the other", t)
		}
		if len(providers) == 0 {
			return fmt.Errorf("no providers found on this box")
		}
		chosen = providers
	} else {
		var err error
		chosen, err = selectTargets(providers, t, include, exclude, interactive)
		if err != nil {
			return err
		}
	}

	// Resolve the release: --tag wins; otherwise fetch latest. The resolved
	// releaseInfo is kept so the tool's own asset digest can be looked up
	// from the SAME release (self-update leg below).
	var rel *releaseInfo
	if cfg.Tag == "" {
		var rerr error
		rel, rerr = latestRelease()
		if rerr != nil {
			return rerr
		}
		cfg.Tag = rel.Tag
		if cfg.Digest == "" {
			cfg.Digest = rel.ProviderDigest
		}
		if cfg.AssetURL == "" {
			cfg.AssetURL = rel.URL
		}
	} else if cfg.Digest == "" {
		// --tag without --digest: resolve the digest from the release API
		// so the download is always verified (never silently skipped —
		// the staged binary would be executed as the provider user).
		var rerr error
		rel, rerr = fetchReleaseByTag(cfg.Tag)
		if rerr != nil {
			return rerr
		}
		cfg.Digest = rel.ProviderDigest
		if cfg.AssetURL == "" {
			cfg.AssetURL = rel.URL
		}
	}

	// Tool self-update asset + digest, resolved from the same release. When
	// the release predates tool assets the digest is empty and the leg skips.
	// A fully explicit --tag+--digest invocation (rel == nil, no API call)
	// cannot resolve the tool digest — skip the self-update leg there rather
	// than failing the provider update.
	toolAsset, terr := runningToolAssetName()
	if terr != nil {
		// Can't even resolve which tool we are; skip the self-update leg
		// rather than risk targeting the wrong asset. Providers still update.
		fmt.Printf("tool self-update skipped (%v)\n", terr)
	} else {
		cfg.ToolAsset = toolAsset
		if rel != nil {
			cfg.ToolDigest = digestForAsset(rel.Assets, cfg.ToolAsset)
		} else {
			fmt.Printf("tool self-update skipped (explicit --digest, no asset list)\n")
		}
	}

	// Confirm version choice interactively unless -f or dry-run already
	// covers it (dry-run prints without acting).
	if !force && !dryRun {
		yes, cerr := confirmVersion(cfg.Tag, chosen)
		if cerr != nil {
			return cerr
		}
		if !yes {
			return nil
		}
	}

	// Confirm once for the whole set, listing every provider.
	ok, err := confirmGateMulti(fmt.Sprintf("update %d provider(s) to %s", len(chosen), cfg.Tag), chosen, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}

	// Validate EVERY provider's preconditions before touching any of them —
	// a single missing binary path must not abort after earlier providers
	// were already updated.
	for _, p := range chosen {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no resolvable binary path — nothing updated", providerLabel(p))
		}
	}

	// Create the private staging dir ONLY now — after dry-run, cancellation,
	// and no-op paths. A dry run or declined confirm must not create (and
	// then remove) a temp dir or fail on staging permissions.
	stageDir, serr := newStageDir()
	if serr != nil {
		return serr
	}
	// Private staging dir is created per-update; always clean it up.
	defer os.RemoveAll(stageDir)
	cfg.StageDir = stageDir

	// Pre-stage THIS release's tool binary so the restart escalation path
	// can run `sudo -n <staged new tool> __do-restart` if the provider unit
	// refuses to restart without privileges. The running binary always
	// predates its own restart-flow fixes; retrying through the staged copy
	// makes this release's fixes live during this very update. Best effort:
	// unavailability only disables the escalation leg.
	stagedTool := stageToolForEscalation(cfg)

	failures := 0
	for _, p := range chosen {
		// Version check: p.Version is the on-disk binary's version, which may
		// differ from the running process. If BinaryDeleted is true, a prior
		// update swapped the binary on disk but the old process is still
		// running from a deleted inode — the version is stale, so do NOT skip.
		// Also re-check /proc/<pid>/exe for the (deleted) marker on Linux
		// (the discovery sample may have been taken before the swap).
		skip := false
		if p.Version == cfg.Tag && !p.BinaryDeleted {
			if runtime.GOOS == "linux" && p.PID > 0 {
				if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.PID)); err == nil {
					_, isDeleted := strings.CutSuffix(exe, " (deleted)")
					skip = !isDeleted
				} else {
					// /proc unreadable (unprivileged cross-user) — cannot
					// determine currency; do NOT skip. A redundant re-install
					// is cheap (digest check short-circuits); a missed update
					// leaves the provider stuck forever.
					skip = false
				}
			} else {
				skip = true
			}
		}
		if skip {
			fmt.Printf("provider %s already on %s\n", providerLabel(p), cfg.Tag)
			continue
		}
		if err := updateProviderWithRestart(p, cfg, stagedTool); err != nil {
			// Continue the batch; report all failures at the end rather
			// than aborting mid-fleet.
			fmt.Fprintf(os.Stderr, "update %s failed: %v\n", providerLabel(p), err)
			failures++
		}
	}

	// Self-update leg: refresh the tool binary itself from the same release.
	// The download+verify already happened above for the escalation path;
	// selfUpdateTool re-downloads ONLY if the staged file is gone or stale,
	// and skips instantly on digest match otherwise. A failure here is
	// reported but does NOT fail the command — providers were the primary
	// job and may have succeeded; the tool can be retried (e.g.
	// `urnet-tools self-update`) without touching providers.
	if err := runToolSelfUpdate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tool self-update failed: %v\n", err)
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d provider(s) failed to update", failures, len(chosen))
	}
	return nil
}

// runToolSelfUpdate executes the tool self-update leg after provider
// updates. It returns the self-update error (which cmdUpdate reports but
// does NOT fail the command on), or nil when the leg was skipped (release
// predates tool assets — nothing verified to install).
func runToolSelfUpdate(cfg updateConfig) error {
	if cfg.ToolDigest == "" {
		fmt.Println("tool self-update skipped (release has no tool asset)")
		return nil
	}
	return selfUpdateTool(cfg)
}

// cmdSelfUpdate updates ONLY the tool binary itself (urnet-tools or
// urnet-docker) to the latest release — no provider discovery, no restart.
// This is the machine/script path for keeping the tool fresh on boxes where
// providers run elsewhere (docker hosts) or where `update`'s provider leg
// should not run.
func cmdSelfUpdate(args []string, force, dryRun bool) error {
	cfg := updateConfig{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tag":
			if i+1 >= len(args) {
				return fmt.Errorf("--tag requires a value")
			}
			cfg.Tag = args[i+1]
			i++
		case "--digest":
			if i+1 >= len(args) {
				return fmt.Errorf("--digest requires a value")
			}
			cfg.ToolDigest = args[i+1]
			i++
		case "--url":
			if i+1 >= len(args) {
				return fmt.Errorf("--url requires a value")
			}
			cfg.ToolAssetURL = args[i+1]
			i++
		case "--help", "-h":
			fmt.Printf("Usage: %s self-update [--tag <version>] [--digest <sha256>]\n", programName())
			return nil
		default:
			return fmt.Errorf("unknown flag %q for self-update (--tag/--digest/--url)", args[i])
		}
	}

	// Resolve the release + tool asset digest.
	var rel *releaseInfo
	if cfg.Tag == "" {
		var err error
		rel, err = latestRelease()
		if err != nil {
			return err
		}
		cfg.Tag = rel.Tag
	} else if cfg.ToolDigest == "" {
		var err error
		rel, err = fetchReleaseByTag(cfg.Tag)
		if err != nil {
			return err
		}
	}
	toolAsset, terr := runningToolAssetName()
	if terr != nil {
		return fmt.Errorf("self-update: %w", terr)
	}
	cfg.ToolAsset = toolAsset
	if rel != nil && cfg.ToolDigest == "" {
		cfg.ToolDigest = digestForAsset(rel.Assets, cfg.ToolAsset)
	}

	if cfg.ToolDigest == "" {
		return fmt.Errorf("self-update: no sha256 digest for %s asset %q; release predates tool assets", cfg.Tag, cfg.ToolAsset)
	}

	if dryRun {
		fmt.Printf("would update %s -> %s (sha256 verified)\n", cfg.ToolAsset, cfg.Tag)
		return nil
	}
	if !force {
		line, err := confirmStdinRead(fmt.Sprintf("Update tool %s to %s? [Y/n]: ", cfg.ToolAsset, cfg.Tag))
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "" && line != "y" && line != "yes" {
			fmt.Println("aborted")
			return nil
		}
	}

	stageDir, err := newStageDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	cfg.StageDir = stageDir

	return selfUpdateTool(cfg)
}

// confirmVersion prompts "update to <tag>?" for the chosen set. Returns
// (true, nil) on yes; (false, nil) on no/abort.
func confirmVersion(tag string, providers []Provider) (bool, error) {
	fmt.Printf("Latest release: %s", tag)
	if len(providers) > 0 {
		fmt.Printf("  (targeting %d provider(s))", len(providers))
	}
	fmt.Println()
	for _, p := range providers {
		fmt.Printf("  %s  current=%s\n", providerLabel(p), orDash(p.Version))
	}
	line, err := confirmStdinRead("Update to this version? [Y/n]: ")
	if err != nil {
		return false, err
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" || line == "y" || line == "yes" {
		return true, nil
	}
	fmt.Println("aborted")
	return false, nil
}

// orDash renders empty strings as "-".
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// forceInteractive reports whether prompts/pickers should be enabled. With
// -f we never prompt — scripts must be fully explicit.
func forceInteractive(force bool) bool {
	return !force && stdinIsInteractive()
}

// splitLabels splits a comma-separated label list.
func splitLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// updateProviderWithRestart wraps updateProvider so its final restart goes
// through restartLadder with THIS update's staged tool binary. When
// stagedTool is empty the restart behaves exactly as before.
func updateProviderWithRestart(p Provider, cfg updateConfig, stagedTool string) error {
	if stagedTool == "" || p.Unit == "" {
		return updateProvider(p, cfg)
	}
	orig := restartForUpdate
	restartForUpdate = func(pp Provider) error {
		return restartLadder(pp, stagedTool)
	}
	defer func() { restartForUpdate = orig }()
	return updateProvider(p, cfg)
}

// updateProvider performs the surgical binary swap for one provider:
//  1. Stage the tarball on real disk (never /tmp tmpfs).
//  2. Verify sha256 against the release digest when provided.
//  3. Extract ONLY linux/$arch/provider — not the whole multi-platform
//     tarball (bloat + the tmpfs overflow root cause).
//  4. Back up the current binary.
//  5. Swap with the provider user's ownership.
//  6. Restart the unit that OWNS the running process (systemd unit name, or
//     fall back to restarting by user-level unit, or plain process signal).
//
// This is the exact recipe proven on 2026-08-09 for taco's fleet.
func updateProvider(p Provider, cfg updateConfig) error {
	// A digest is MANDATORY: without it the downloaded binary is executed
	// (version check + install, often as the provider user) with no
	// integrity verification. Check BEFORE any staging side effects — the
	// flag parser and release lookup both ensure a digest is resolved;
	// this is the last line of defense.
	if cfg.Digest == "" {
		return fmt.Errorf("update: no sha256 digest for %s; refusing unverified download", cfg.Tag)
	}
	if err := os.MkdirAll(cfg.StageDir, 0o755); err != nil {
		return fmt.Errorf("stage dir: %w", err)
	}
	arch := runtimeGOARCH()
	relPath := tarRelPath(runtime.GOOS, arch)

	url := cfg.AssetURL
	if url == "" {
		url = fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/urnetwork-provider-%s.tar.gz", cfg.Tag, cfg.Tag)
	}
	tarball := filepath.Join(cfg.StageDir, cfg.Tag+".tar.gz")

	fmt.Printf("downloading %s\n", url)
	if err := downloadFile(url, tarball); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := verifySHA256(tarball, cfg.Digest); err != nil {
		return err
	}
	fmt.Println("sha256 verified")

	// Extract only the needed arch's provider binary.
	extractDir := filepath.Join(cfg.StageDir, "extract-"+cfg.Tag)
	if err := os.RemoveAll(extractDir); err != nil {
		return err
	}
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	if err := extractSingleFile(tarball, relPath, filepath.Join(extractDir, "provider")); err != nil {
		return fmt.Errorf("extract %s: %w", relPath, err)
	}

	// Structural sanity-check the staged binary WITHOUT executing it.
	// Running a freshly downloaded artifact (e.g. `staged --version`) is
	// code execution of a remote file — the same class of defect the hub
	// path guards with isRecognizedExecutable. sha256
	// already guarantees the artifact matches the requested tag, so an
	// ELF/Mach-O/PE magic check is the right ceiling here: it confirms we
	// extracted a real binary for this platform, not a script or corrupted
	// file.
	staged := filepath.Join(extractDir, "provider")
	if !isRecognizedExecutable(staged) {
		return fmt.Errorf("staged binary %s is not a %s executable (corrupted or wrong asset) — refusing to install", staged, runtime.GOOS)
	}

	// Backup current binary with a nanosecond-timestamped name so repeated
	// updates never collide.
	//
	// The running provider binary may have been deleted on disk already by a
	// prior interrupted/partial update. Discover() reads /proc/<pid>/exe which,
	// for a deleted running binary, resolves to a "<path> (deleted)" symlink
	// target whose on-disk file does not exist. There is then nothing to back
	// up — skip it (warn) and install the fresh binary rather than failing the
	// whole update on a phantom backup path.
	// Prune old backups before creating a new one so disk pressure cannot
	// cause the update to fail .
	pruneBackups(p.Binary, 2)
	if _, err := os.Stat(p.Binary); os.IsNotExist(err) {
		fmt.Printf("note: current binary %s no longer exists on disk (deleted by a prior update); skipping backup\n", p.Binary)
	} else {
		backup := backupName(p.Binary, time.Now())
		if _, err := os.Stat(backup); err == nil {
			// Same-instant collision: fail loudly rather than silently reusing
			// the older backup and losing the immediate previous binary.
			return fmt.Errorf("backup %s already exists — refusing to overwrite; retry", backup)
		} else if !os.IsNotExist(err) {
			// Non-NotExist error (permissions, etc.) — treat as a real
			// failure, not "already backed up".
			return fmt.Errorf("backup stat: %w", err)
		}
		if err := copyFile(p.Binary, backup); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		fmt.Printf("backed up %s -> %s\n", p.Binary, backup)
	}
	// Prune old backups after the new one is written, regardless of
	// update outcome .
	defer pruneBackups(p.Binary, 2)

	// Swap with ownership preserved for the provider user.
	if err := installBinary(staged, p.Binary, p.User); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	fmt.Printf("swapped %s -> %s\n", staged, p.Binary)

	// Restart the unit that owns the running process. restartForUpdate is
	// plain restartProvider by default; the update flow temporarily routes
	// it through the staged-tool escalation ladder
	// (updateProviderWithRestart).
	if err := restartForUpdate(p); err != nil {
		return fmt.Errorf("restart %s: %w", providerLabel(p), err)
	}

	// Verify the restart took effect: wait for the process to be on the
	// new version. The on-disk binary IS the new version (we just swapped
	// it), so we re-discover and check that the running PID changed and
	// the new process reports the expected version.
	oldPID := p.PID
	for i := 0; i < 15; i++ { // up to ~30s
		time.Sleep(2 * time.Second)
		providers := Discover()
		for _, rp := range providers {
			if rp.StateDir == p.StateDir && rp.StateDir != "" && rp.PID != 0 && rp.PID != oldPID && rp.Version == cfg.Tag {
				fmt.Printf("verified %s running %s (pid %d)\n", providerLabel(p), cfg.Tag, rp.PID)
				pruneBackups(p.Binary, 2)
				return nil
			}
		}
	}
	return fmt.Errorf("update %s: binary swapped to %s but restart did not take effect — the running process is still the old version or the unit failed to start; check the provider's logs", providerLabel(p), cfg.Tag)
}

// restartProvider restarts the systemd unit (system or user level) that owns
// the provider process. Falls back gracefully when systemd is unavailable.
func restartProvider(p Provider) error {
	if p.Unit != "" {
		// Determine the unit's real scope up front (isUserUnit checks whether
		// a systemd system unit file exists). A user-owned unit MUST be
		// restarted in the owning user's --user session first: restarting it
		// via the system scope prompts for root/polkit (systemd1.manage-units)
		// or fails outright non-interactively. Only treat it as a system unit
		// when there is genuinely a system unit file. This was the bug: the
		// previous order tried the SYSTEM scope first, so a user-owned
		// provider demanded the root password on some boxes and failed
		// outright on others.
		userScoped := isUserUnit(p.Unit)
		if userScoped && p.User != "" {
			// Restart in the owning user's --user session (no root/polkit).
			args := append([]string{"systemctl"}, systemctlUserArgs(p.User)...)
			args = append(args, "restart", p.Unit)
			out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
			if err == nil {
				fmt.Printf("restarted %s (user %s)\n", p.Unit, p.User)
				return nil
			}
			// If the user session is unreachable (not logged in / no manager),
			// fall through to the PID-signal fallback below rather than
			// demanding root or erroring the whole update.
			if !strings.Contains(string(out), "not found") && !strings.Contains(string(out), "No such") &&
				!strings.Contains(string(out), "Could not get properties") && !strings.Contains(string(out), "Failed to connect") {
				return fmt.Errorf("systemctl --user restart %s: %v (%s)", p.Unit, err, strings.TrimSpace(string(out)))
			}
		} else if userScoped && p.User == "" {
			// Unit looks user-scoped but no owning user was resolved (e.g.
			// no passwd entry, or system unit with no User= directive).
			// Try system scope as a fallback — it costs one failed systemctl
			// at worst, and avoids the "neither branch matches" dead zone.
			out, err := exec.Command("systemctl", "restart", p.Unit).CombinedOutput()
			if err == nil {
				fmt.Printf("restarted %s (system fallback, user unknown)\n", p.Unit)
				return nil
			}
			if !strings.Contains(string(out), "not found") && !strings.Contains(string(out), "No such") {
				return fmt.Errorf("unit %s looks user-scoped but no owning user resolved; system restart failed: %v (%s)",
					p.Unit, err, strings.TrimSpace(string(out)))
			}
		} else if !userScoped {
			// Genuinely system-owned unit: restart via the system manager.
			out, err := exec.Command("systemctl", "restart", p.Unit).CombinedOutput()
			if err == nil {
				fmt.Printf("restarted %s\n", p.Unit)
				return nil
			}
			if !strings.Contains(string(out), "not found") && !strings.Contains(string(out), "No such") {
				return fmt.Errorf("systemctl restart %s: %v (%s)", p.Unit, err, strings.TrimSpace(string(out)))
			}
		}
	}
	if p.User != "" && p.PID > 0 {
		// No systemd ownership resolved: do NOT signal the process.
		// Sending SIGINT to a bare process without a unit just kills it
		// permanently — the provider won't restart. Return an
		// actionable error so the operator can restart manually.
		return fmt.Errorf("no systemd unit resolved for %s — restart the provider manually (pid %d)", providerLabel(p), p.PID)
	}
	return fmt.Errorf("could not restart provider %s — restart the owning unit manually", providerLabel(p))
}

// downloadFile fetches url into path (atomic-ish: temp file + rename).
func downloadFile(url, path string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// verifySHA256 checks the file's sha256 against the expected hex digest.
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

// tarRelPath returns the in-archive path of the provider binary for goos.
//
// Tar headers always use forward slashes regardless of host OS; using
// filepath.Join here would produce backslashes on Windows and the
// in-archive lookup would never match.
func tarRelPath(goos, arch string) string {
	if goos == "windows" {
		return path.Join("windows", arch, "provider.exe")
	}
	return path.Join("linux", arch, "provider")
}

// extractSingleFile extracts exactly one file from a .tar.gz to dst.
func extractSingleFile(tarball, relPath, dst string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Name == relPath || strings.TrimPrefix(hdr.Name, "./") == relPath {
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			out.Close()
			return cerr
		}
	}
	return fmt.Errorf("path %s not found in tarball", relPath)
}

// installBinary copies src to dst preserving ownership for the given user,
// then atomically renames into place.
//
// installBinary stages a new binary from src to dst using an unpredictable
// temp name (os.CreateTemp) to prevent symlink-planting attacks. The write
// goes to a temp file (same filesystem) and is os.Rename'd over dst — never
// O_TRUNC in place. The running provider may still be executing from dst
// during an update; overwriting that inode in place risks SIGBUS/SIGSEGV on
// demand-paging, while rename(2) leaves the old inode serving already-open
// processes and only new execve's see the new file.
func installBinary(src, dst, user string) error {
	// M1 fix: use unpredictable temp name instead of predictable dst+".new"
	// to prevent symlink-planting attacks by a lower-privileged user.
	tmpFile, err := os.CreateTemp(filepath.Dir(dst), "urnetwork-*.new-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	newPath := tmpFile.Name()
	tmpFile.Close() // close before copyFile reopens for writing

	if err := copyFile(src, newPath); err != nil {
		os.Remove(newPath)
		return err
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		os.Remove(newPath)
		return err
	}
	if user != "" && os.Geteuid() == 0 {
		uid, gid, err := lookupUserIDs(user)
		if err != nil {
			os.Remove(newPath)
			return fmt.Errorf("resolve uid/gid for %s: %w", user, err)
		}
		if err := os.Chown(newPath, uid, gid); err != nil {
			os.Remove(newPath)
			return fmt.Errorf("chown %s: %w", newPath, err)
		}
	}
	if err := os.Rename(newPath, dst); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", newPath, dst, err)
	}
	// Sync the parent directory so the rename survives a crash before
	// the directory's own metadata is committed. Without this, a power
	// loss after rename but before the dir entry is persisted can lose
	// the new binary path.
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

// backupName returns the timestamped backup path for a binary — distinct
// names even for repeated updates within the same second are NOT guaranteed
// at second resolution, but the timestamp carries the wall clock so backups
// never collide across seconds. Extracted as a pure
// helper so tests call production logic.
func backupName(binary string, at time.Time) string {
	return binary + ".bak-" + at.UTC().Format("20060102T150405.000000000Z")
}

// pruneBackups removes older .bak-* backups for a binary, keeping the
// newest 'keep' entries. Best-effort: removal errors are logged but never
// fail the caller. Called before writing a new backup (to free disk space)
// and via defer after updateProvider returns (to handle success and failure
// paths uniformly).
func pruneBackups(binaryPath string, keep int) {
	dir := filepath.Dir(binaryPath)
	base := filepath.Base(binaryPath)
	matches, _ := filepath.Glob(filepath.Join(dir, base+".bak-*"))
	sort.Strings(matches)
	if len(matches) > keep {
		for _, old := range matches[:len(matches)-keep] {
			os.Remove(old)
		}
	}
}

// copyFile copies src to dst preserving mode.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if cerr != nil {
		out.Close()
		return cerr
	}
	// Fsync the file contents (data durability) before close. On ext4
	// data=ordered the rename is ordered after data, so this is usually safe
	// without Sync, but on other filesystems and on power loss the risk is a
	// zero-length provider binary at the canonical path.
	if err := fsyncFile(out); err != nil {
		out.Close()
		return fmt.Errorf("fsync %s: %w", dst, err)
	}
	// Close flushes buffered data — a disk-full or I/O error here would
	// otherwise be lost and the copy reported successful.
	if cerr = out.Close(); cerr != nil {
		return fmt.Errorf("close %s: %w", dst, cerr)
	}
	return nil
}

// toolAssetName returns the release asset name for a tool binary:
// <base>-<os>-<arch> (e.g. urnet-tools-linux-amd64). Release assets are
// bare binaries — never a .exe suffix, even on Windows.
func toolAssetName(base, goos, arch string) string {
	return fmt.Sprintf("%s-%s-%s", base, goos, arch)
}

// programName returns the invoked binary's base name without a .exe suffix
// (urnet-tools or urnet-docker). Used for help text and messages that must
// name the tool the user actually invoked — a hardcoded "urnet-tools" is
// wrong when urnet-docker routes into the same code (verified 2026-08-12
// review).
func programName() string {
	base := "urnet-tools"
	if exe, err := os.Executable(); err == nil {
		if b := filepath.Base(exe); b != "" {
			base = strings.TrimSuffix(b, ".exe")
		}
	}
	return base
}

// runningToolAssetName derives the asset name from the ACTUAL running
// binary's base name, so the same code serves urnet-tools and urnet-docker
// without hardcoding which tool is updating. On Windows os.Executable()
// returns a name ending in .exe; the trailing .exe is stripped because
// release assets are bare (a mismatch here would look for the wrong asset —
// verified 2026-08-12 review). Returns an error rather than silently
// falling back to a default base name, which could target the WRONG tool's
// asset (urnet-tools vs urnet-docker).
func runningToolAssetName() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable: %w", err)
	}
	base := filepath.Base(exe)
	// Strip a trailing .exe — Windows-convention: os.Executable() returns
	// the .exe name there, but release assets are bare. On non-Windows hosts
	// this is a no-op for any real tool binary (none ship with .exe names).
	base = strings.TrimSuffix(base, ".exe")
	if base == "" {
		return "", fmt.Errorf("empty executable base name for %q", exe)
	}
	return toolAssetName(base, runtime.GOOS, runtimeGOARCH()), nil
}

// toolAssetURL is the release download URL for a tool asset.
func toolAssetURL(tag, asset string) string {
	// M3 fix: escape tag in download URL to prevent path traversal.
	return fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/%s", url.PathEscape(tag), asset)
}

// selfUpdateTool updates the running tool binary (urnet-tools or
// urnet-docker) in place from the release's tool asset. This is what makes
// the Go tool self-sustaining: `update` refreshes BOTH the providers AND the
// tool binary, so a box never needs the shell installer again.
//
// The flow mirrors updateProvider's safety shape: digest MANDATORY, sha256
// verify, ELF structural check, timestamped backup, atomic rename swap. The
// one deliberate difference: if the release predates tool assets (no digest),
// we SKIP with a notice instead of refusing the whole update — an old
// release can still update providers without a tool asset.
func selfUpdateTool(cfg updateConfig) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	return selfUpdateToolTo(exe, cfg)
}

// selfUpdateToolTo is the testable core of selfUpdateTool: swap the binary
// at exePath (not necessarily os.Executable) from cfg.ToolAssetURL with
// cfg.ToolDigest verification. See selfUpdateTool for the skip semantics.
func selfUpdateToolTo(exePath string, cfg updateConfig) error {
	if cfg.StageDir == "" {
		return fmt.Errorf("self-update: stage dir required (real disk, not /tmp)")
	}
	if cfg.ToolDigest == "" {
		// Release predates tool assets (pre-v3.23.0-fix.28). Skipping is
		// correct — there is nothing verified to install. Providers still
		// update via the tarball path. This is a skip, not a failure: the
		// caller (cmdUpdate) checks ToolDigest != "" before invoking and
		// prints "skipped" itself; this guard exists for direct callers.
		return fmt.Errorf("no sha256 digest for %s asset %q; release predates tool assets — nothing to update", cfg.Tag, cfg.ToolAsset)
	}
	if err := os.MkdirAll(cfg.StageDir, 0o755); err != nil {
		return fmt.Errorf("stage dir: %w", err)
	}

	// Skip when already current: compare the installed binary against the
	// release digest BEFORE downloading (no point re-downloading ourselves).
	if cur, err := fileSHA256(exePath); err == nil && strings.EqualFold(cur, cfg.ToolDigest) {
		fmt.Printf("tool %s already on %s\n", cfg.ToolAsset, cfg.Tag)
		return nil
	}

	// A previous update's .old file may still exist (Windows keeps it locked
	// until the updater process exits). Sweep it now — we are running, so the
	// prior updater has exited.
	cleanupStaleBackups(exePath)

	url := cfg.ToolAssetURL
	if url == "" {
		url = toolAssetURL(cfg.Tag, cfg.ToolAsset)
	}
	staged := filepath.Join(cfg.StageDir, cfg.ToolAsset)
	// Reuse an already-staged copy when its digest matches (the update flow
	// pre-stages this exact asset for the restart-escalation path — never
	// download the same verified file twice).
	if cur, serr := fileSHA256(staged); serr == nil && strings.EqualFold(cur, cfg.ToolDigest) {
		fmt.Println("staged tool already verified")
	} else {
		fmt.Printf("downloading %s\n", url)
		if err := downloadFile(url, staged); err != nil {
			return fmt.Errorf("download tool: %w", err)
		}
		if err := verifySHA256(staged, cfg.ToolDigest); err != nil {
			return err
		}
		fmt.Println("tool sha256 verified")
	}
	if !isRecognizedExecutable(staged) {
		return fmt.Errorf("staged tool %s is not a %s executable (corrupted or wrong asset) — refusing to install", staged, runtime.GOOS)
	}

	backup := backupName(exePath, time.Now())
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("backup %s already exists — refusing to overwrite; retry", backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup stat: %w", err)
	}

	// Windows-safe swap: rename the running executable ASIDE first, then
	// install the staged binary at the freed path. On Windows you cannot
	// rename a .new over a running .exe (the kernel locks the image section),
	// but you CAN rename the running executable itself to a .bak-* name — the
	// standard self-update pattern. On failure the original is restored from
	// .bak-*; on success the backup is left for removal after process exit
	// (the next invocation cleans stale .bak-* files).
	if err := os.Rename(exePath, backup); err != nil {
		return fmt.Errorf("move current tool aside: %w", err)
	}
	fmt.Printf("moved %s -> %s\n", exePath, backup)

	// Swap in place. The tool runs as the invoking user (root or the
	// operator), so no chown is needed — installBinary with user="" keeps
	// current ownership.
	if err := installBinary(staged, exePath, ""); err != nil {
		// Restore the original before giving up — an update that fails
		// mid-swap must not leave the tool missing.
		if rerr := os.Rename(backup, exePath); rerr != nil {
			return fmt.Errorf("swap tool binary: %w (restore from %s failed: %v)", err, backup, rerr)
		}
		return fmt.Errorf("swap tool binary: %w", err)
	}
	fmt.Printf("updated %s -> %s\n", cfg.ToolAsset, cfg.Tag)
	return nil
}

// cleanupStaleBackups removes .bak-* files left by earlier self-update
// swaps. On Windows a .old file stays locked by the process that created it
// until that process exits; because THIS process is running, any previous
// updater has exited and its .old is removable. Best-effort only — cleanup
// must never fail an update.
func cleanupStaleBackups(exePath string) {
	matches, err := filepath.Glob(exePath + ".bak-*")
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// fileSHA256 returns the hex sha256 of a file ("" on error).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
