package urnettools

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// isHotSwapSupportedVersion checks whether the given provider version string indicates
// support for in-process zero-downtime HotSwap (introduced in v3.23.0-fix.31.0).
//
// The check is anchored to the "3.23.0-fix." base version, not just the
// presence of "fix." anywhere in the string: an unanchored substring match
// would also accept a pre-fork or differently-based version like
// "v3.22.0-fix.31.0" that happens to contain "fix.31", and triggerHotSwap
// sends SIGUSR2 unconditionally once this returns true — a provider that
// predates the HotSwap feature has no handler for that signal and would
// terminate under its default action instead of gracefully handing off.
func isHotSwapSupportedVersion(ver string) bool {
	if ver == "" {
		return false
	}
	// The only non-numeric override this recognizes: local/CI dev builds
	// report exactly "dev" (see RequireVersion()'s fallback), which always
	// supports the current in-tree HotSwap implementation.
	if ver == "dev" {
		return true
	}
	ver = strings.TrimPrefix(ver, "v")
	const base = "3.23.0-fix."
	if !strings.HasPrefix(ver, base) {
		return false
	}
	sub := ver[len(base):]
	dotIdx := strings.IndexAny(sub, ".-")
	if dotIdx != -1 {
		sub = sub[:dotIdx]
	}
	fixNum, err := strconv.Atoi(sub)
	if err != nil {
		return false
	}
	return fixNum >= 31
}

// supportsHotSwap determines whether the target running provider is capable of
// zero-downtime HotSwap handoff. Both the reported version AND (when the
// provider is systemd-managed) the owning unit's Type= must check out —
// see hotSwapVersionOK and hotSwapUnitOK for why each is necessary on its
// own.
func supportsHotSwap(p Provider) bool {
	return hotSwapPreflight(p) == nil
}

// ErrHotSwapNotSupported is returned when the running provider process does not support zero-downtime hotswap.
var ErrHotSwapNotSupported = errors.New("running provider does not support zero-downtime hotswap (requires >= v3.23.0-fix.31.0)")

// ErrHotSwapUnitNotNotify is returned when the provider's version supports
// HotSwap but its owning systemd unit is not Type=notify, so
// provider/hotswap.go would abort the in-process handoff internally rather
// than actually hand off (see hotSwapUnitOK for the mechanism). Naming a
// version here would be actively misleading: the fix is rewriting the
// systemd unit, not upgrading the binary. The distinction matters because
// the two obvious candidate remedies do NOT work. cmdUpdate and
// cmdReinstall both route through updateProvider, which re-fetches and
// atomically installs the binary and restarts the unit but never writes a
// unit file, so neither migrates a Type=simple node. Only re-running
// install_systemd_units in Provider_Install_Linux.sh does.
var ErrHotSwapUnitNotNotify = errors.New("provider's systemd unit is not Type=notify, so zero-downtime hotswap cannot complete; re-run the installer script (Provider_Install_Linux.sh) to rewrite the unit with Type=notify. Note neither `urnet-tools update` nor `urnet-tools reinstall` rewrites the unit: both only re-fetch the binary")

// hotSwapPreflight reports WHY the handoff cannot run, or nil when it can.
// It is the single source of that decision: triggerHotSwap calls it before
// signalling, and updateProvider calls it to decide whether to try at all,
// so the operator-facing decline text is identical on both paths and no
// caller can gate on a bool and lose the reason.
func hotSwapPreflight(p Provider) error {
	if !hotSwapVersionOK(p) {
		return ErrHotSwapNotSupported
	}
	if err := hotSwapUnitOK(p); err != nil {
		return err
	}
	return nil
}

// hotSwapVersionOK reports whether the provider's running image or reported
// version is new enough to speak the HotSwap handoff protocol at all.
func hotSwapVersionOK(p Provider) bool {
	if p.PID > 0 {
		if exe, err := runningImagePath(p.PID); err == nil {
			// providerVersion, not the buildinfo-only variant: -trimpath
			// release builds carry no main.Version in buildinfo, so the
			// buildinfo-only read fell through to the module pseudo-version
			// (v0.0.0-<ts>-<rev>). That is non-empty, so this branch took it
			// and handed isHotSwapSupportedVersion a string that can never
			// parse as a release, leaving supportsHotSwap permanently false
			// on every release binary.
			if ver := providerVersion(exe); ver != "" {
				return isHotSwapSupportedVersion(ver)
			}
		}
	}
	if p.Version != "" {
		return isHotSwapSupportedVersion(p.Version)
	}
	return false
}

// unitTypeFunc resolves a provider's owning systemd unit Type= value.
// Overridable so tests can exercise hotSwapUnitOK without a real systemd.
var unitTypeFunc = queryUnitType

// queryUnitType runs `systemctl show -p Type --value <unit>`, scoped
// EXACTLY like restartProvider/unitCommandArgs: a user-owned unit is
// queried in the owning user's own --user session (never the system
// manager, and never cross-user without -M). Getting this scope wrong is
// what causes the polkit/root prompts restartProvider's own comments call
// out — reuse that determination (isUserUnit + systemctlUserArgs) rather
// than re-deriving it.
func queryUnitType(p Provider) (string, error) {
	if p.Unit == "" {
		return "", fmt.Errorf("provider %s has no owning systemd unit", providerLabel(p))
	}
	var args []string
	if isUserUnit(p.Unit) && p.User != "" {
		args = append([]string{"systemctl"}, systemctlUserArgs(p.User)...)
		args = append(args, "show", "-p", "Type", "--value", p.Unit)
	} else {
		args = []string{"systemctl", "show", "-p", "Type", "--value", p.Unit}
	}
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show -p Type %s: %w", p.Unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hotSwapUnitOK reports nil when the provider's systemd unit (if any) allows
// the HotSwap handoff to actually complete, and otherwise the reason it does
// not: ErrHotSwapUnitNotNotify for a unit whose Type= was read and is not
// notify, or a wrapped query error when the type could not be read at all. provider/hotswap.go's Unix
// branch aborts the handoff whenever INVOCATION_ID is set (i.e. systemd
// started the process) and NOTIFY_SOCKET is empty — that only happens for
// a unit that isn't Type=notify, since NotifyAccess=all + Type=notify is
// what puts NOTIFY_SOCKET in the environment. Every pre-existing fleet node
// still runs Type=simple (only install_systemd_units in
// Provider_Install_Linux.sh writes Type=notify, and `urnet-tools update`
// only ever swaps the binary, never the unit), so without this check
// supportsHotSwap said "yes" purely from the version string,
// triggerHotSwap fired, provider/hotswap.go silently aborted the internal
// handoff, and update.go's "hotSwapTriggered = true" skipped the
// restartForUpdate fallback entirely — turning every update into a
// permanent no-op on pre-existing nodes.
//
// A provider not managed by systemd at all (no p.Unit — e.g. the Docker
// PID-1 in-place execve path) never reaches that INVOCATION_ID check in the
// first place (see provider/hotswap.go's Docker branch, which runs before
// it), so it keeps working here unconditionally.
//
// When the unit type genuinely cannot be determined (systemctl missing,
// permission error, unit vanished mid-check) this deliberately returns
// false rather than true: a false "no" only costs zero-downtime and falls
// through to update.go's normal restartForUpdate fallback, which always
// works; a false "yes" fires triggerHotSwap into a handoff that silently
// aborts and, per the paragraph above, bricks the update entirely. Between
// those two failure modes, losing zero-downtime is always the safe one.
func hotSwapUnitOK(p Provider) error {
	if p.Unit == "" {
		return nil
	}
	typ, err := unitTypeFunc(p)
	if err != nil {
		// Do NOT collapse this into ErrHotSwapUnitNotNotify. That error tells
		// the operator to re-run the installer to rewrite the unit, which is
		// the wrong remedy and a false diagnosis when the unit's Type= was
		// never actually read: systemctl missing, the user bus unreachable,
		// or a polkit denial all land here. Report what failed instead.
		return fmt.Errorf("query systemd unit type for %s: %w", p.Unit, err)
	}
	if typ != "notify" {
		return ErrHotSwapUnitNotNotify
	}
	return nil
}

// cmdHotswap implements `urnet-tools hotswap [target]`: it triggers an in-process
// zero-downtime binary handover on a running provider without cycling the unit.
func cmdHotswap(args []string, force, dryRun bool) error {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}
	providers := lifecycleCandidates(t)
	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("hotswap takes no arguments (got %v)", rest)
	}
	if !p.Running || p.PID <= 0 {
		return fmt.Errorf("provider %s is not running (cannot hot-swap)", providerLabel(p))
	}
	ok, err := confirmGate("zero-downtime hot-swap "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run or declined
	}
	if err := triggerHotSwap(p); err != nil {
		return fmt.Errorf("hotswap %s: %w", providerLabel(p), err)
	}
	fmt.Printf("triggered zero-downtime HotSwap on %s (PID %d)\n", providerLabel(p), p.PID)
	return nil
}
