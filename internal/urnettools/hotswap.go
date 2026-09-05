package urnettools

import (
	"fmt"
)

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
