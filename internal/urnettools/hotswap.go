package urnettools

import (
	"fmt"
	"strconv"
	"strings"
)

// isHotSwapSupportedVersion checks whether the given provider version string indicates
// support for in-process zero-downtime HotSwap (introduced in v3.23.0-fix.31.0).
func isHotSwapSupportedVersion(ver string) bool {
	if ver == "" {
		return false
	}
	ver = strings.TrimPrefix(ver, "v")
	idx := strings.Index(ver, "fix.")
	if idx != -1 {
		sub := ver[idx+4:]
		dotIdx := strings.IndexAny(sub, ".-")
		if dotIdx != -1 {
			sub = sub[:dotIdx]
		}
		fixNum, err := strconv.Atoi(sub)
		if err == nil {
			return fixNum >= 31
		}
	}
	if strings.Contains(ver, "hotswap") || strings.Contains(ver, "test") || ver == "dev" {
		return true
	}
	return false
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
