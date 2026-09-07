package urnettools

import (
	"fmt"
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
// zero-downtime HotSwap handoff based on its running image or reported version.
func supportsHotSwap(p Provider) bool {
	if p.PID > 0 {
		if exe, err := runningImagePath(p.PID); err == nil {
			if ver := providerVersionFromBuildinfo(exe); ver != "" {
				return isHotSwapSupportedVersion(ver)
			}
		}
	}
	if p.Version != "" {
		return isHotSwapSupportedVersion(p.Version)
	}
	return false
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
