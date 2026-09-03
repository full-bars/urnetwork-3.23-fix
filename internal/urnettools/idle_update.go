package urnettools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IdleUpdateOptions holds the tuning knobs for waiting for a traffic lull.
type IdleUpdateOptions struct {
	Threshold uint64        // Max billable bytes/second to consider "idle" (default 5120 = 5 KiB/s)
	Window    time.Duration // Duration traffic must stay below threshold (default 5m)
	Timeout   time.Duration // Maximum duration to wait before proceeding with update (default 30m; 0 = infinite)
}

// DefaultIdleUpdateOptions returns production-safe defaults.
func DefaultIdleUpdateOptions() IdleUpdateOptions {
	return IdleUpdateOptions{
		Threshold: 5120,             // 5 KiB/s
		Window:    5 * time.Minute,  // 300s
		Timeout:   30 * time.Minute, // 30m ceiling
	}
}

// readBillableRateFile reads the current billable rate in bytes/sec from the given state dir.
// Returns (rate, found, err).
func readBillableRateFile(stateDir string) (uint64, bool, error) {
	ratePath := filepath.Join(stateDir, "billable_rate")
	data, err := os.ReadFile(ratePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	s := strings.TrimSpace(string(data))
	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("malformed billable_rate %q: %w", s, err)
	}
	return val, true, nil
}

// formatRate formats bytes/second for human reading.
func formatRate(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B/s", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB/s", float64(b)/float64(div), "KMGTPE"[exp])
}

// waitForIdle waits until the provider's billable traffic drops below opts.Threshold
// for opts.Window, or until opts.Timeout expires.
// pollFn is an optional override for reading the rate (used in unit tests / docker exec).
func waitForIdle(ctx context.Context, stateDir string, running bool, opts IdleUpdateOptions, pollFn func() (uint64, bool, error)) error {
	if opts.Window == 0 {
		fmt.Printf("[idle-update] Window=0 requested — proceeding with immediate update.\n")
		return nil
	}

	if pollFn == nil {
		pollFn = func() (uint64, bool, error) {
			return readBillableRateFile(stateDir)
		}
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	startTime := time.Now()
	var quietDuration time.Duration
	pollInterval := 5 * time.Second

	fmt.Printf("[idle-update] Waiting for billable traffic to drop below %s for %s (timeout: %s)...\n",
		formatRate(opts.Threshold), opts.Window, opts.Timeout)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	missingCount := 0

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fmt.Printf("[idle-update] Timeout of %s reached waiting for idle traffic — proceeding with update.\n", opts.Timeout)
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
		}

		elapsed := time.Since(startTime)
		rate, found, err := pollFn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[idle-update] warning reading billable_rate: %v\n", err)
			quietDuration = 0
			continue
		}

		if !found {
			if !running {
				// Provider is stopped; traffic is guaranteed to be 0
				fmt.Printf("[idle-update] Provider is not running — traffic is zero, proceeding.\n")
				return nil
			}
			missingCount++
			if missingCount < 3 {
				// Newly booted provider may take up to 15s to emit its first tick
				fmt.Printf("[idle-update] Waiting for initial rate metric (%ds)...\n", missingCount*5)
			} else {
				fmt.Printf("[idle-update] billable_rate metric not found; assuming active traffic...\n")
			}
			quietDuration = 0
			continue
		}

		missingCount = 0

		if rate <= opts.Threshold {
			quietDuration += pollInterval
			remaining := opts.Window - quietDuration
			if remaining < 0 {
				remaining = 0
			}
			timeoutLeft := ""
			if opts.Timeout > 0 {
				timeoutLeft = fmt.Sprintf(", timeout in %s", (opts.Timeout - elapsed).Round(time.Second))
			}
			fmt.Printf("  [idle-update] rate: %s — quiet for %s (need %s%s)\n",
				formatRate(rate), quietDuration, remaining, timeoutLeft)

			if quietDuration >= opts.Window {
				// Sustained verification: 5 quick 1s checks
				fmt.Printf("  [idle-update] Threshold met. Verifying sustained quiet (5s check)...\n")
				verified := true
				for i := 0; i < 5; i++ {
					select {
					case <-ctx.Done():
						if errors.Is(ctx.Err(), context.DeadlineExceeded) {
							fmt.Printf("[idle-update] Timeout of %s reached waiting for idle traffic — proceeding with update.\n", opts.Timeout)
							return nil
						}
						return ctx.Err()
					case <-time.After(1 * time.Second):
					}
					vrate, vfound, verr := pollFn()
					if verr != nil || !vfound || vrate > opts.Threshold {
						verified = false
						break
					}
				}
				if verified {
					fmt.Printf("[idle-update] Traffic lull verified (%s). Proceeding with update.\n", formatRate(rate))
					return nil
				}
				// Spiked during verification: reduce quietDuration by 30s rather than zeroing out 5 minutes
				if quietDuration > 30*time.Second {
					quietDuration -= 30 * time.Second
				} else {
					quietDuration = 0
				}
				fmt.Printf("  [idle-update] Transient spike detected during verification; resuming wait (quiet: %s)...\n", quietDuration)
			}
		} else {
			if quietDuration > 0 {
				fmt.Printf("  [idle-update] Traffic active: %s (> %s) — quiet timer reset\n",
					formatRate(rate), formatRate(opts.Threshold))
			}
			quietDuration = 0
		}
	}
}

// parseIdleArgs parses --threshold, --window, and --timeout from args, returning the remaining args.
func parseIdleArgs(args []string) (IdleUpdateOptions, []string, error) {
	opts := DefaultIdleUpdateOptions()
	var remaining []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--threshold":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--threshold requires a value")
			}
			val, err := strconv.ParseUint(args[i+1], 10, 64)
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --threshold %q: %w", args[i+1], err)
			}
			opts.Threshold = val
			i++
		case strings.HasPrefix(arg, "--threshold="):
			val, err := strconv.ParseUint(strings.TrimPrefix(arg, "--threshold="), 10, 64)
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --threshold: %w", err)
			}
			opts.Threshold = val
		case arg == "--window":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--window requires a value")
			}
			dur, err := parseSecondsOrDuration(args[i+1])
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --window %q: %w", args[i+1], err)
			}
			opts.Window = dur
			i++
		case strings.HasPrefix(arg, "--window="):
			dur, err := parseSecondsOrDuration(strings.TrimPrefix(arg, "--window="))
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --window: %w", err)
			}
			opts.Window = dur
		case arg == "--timeout":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--timeout requires a value")
			}
			dur, err := parseSecondsOrDuration(args[i+1])
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --timeout %q: %w", args[i+1], err)
			}
			opts.Timeout = dur
			i++
		case strings.HasPrefix(arg, "--timeout="):
			dur, err := parseSecondsOrDuration(strings.TrimPrefix(arg, "--timeout="))
			if err != nil {
				return opts, nil, fmt.Errorf("invalid --timeout: %w", err)
			}
			opts.Timeout = dur
		default:
			remaining = append(remaining, arg)
		}
	}
	return opts, remaining, nil
}

// parseSecondsOrDuration parses an integer number of seconds or a Go duration string (e.g. "300", "5m", "1h").
func parseSecondsOrDuration(s string) (time.Duration, error) {
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		if sec < 0 {
			return 0, fmt.Errorf("duration cannot be negative")
		}
		return time.Duration(sec) * time.Second, nil
	}
	return time.ParseDuration(s)
}

// cmdIdleUpdate waits for an idle traffic window before invoking cmdUpdate.
func cmdIdleUpdate(args []string, force, dryRun bool) error {
	opts, rest, err := parseIdleArgs(args)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("[dry-run] would wait for idle traffic: threshold=%s, window=%s, timeout=%s\n",
			formatRate(opts.Threshold), opts.Window, opts.Timeout)
		return cmdUpdate(rest, force, dryRun)
	}

	// Resolve target provider(s) to find state dir
	t, _, _ := parseTargetFlagsLenient(rest)
	providers := Discover()
	chosen, err := selectTargets(providers, t, nil, nil, false)
	if err != nil {
		return err
	}
	if len(chosen) > 0 {
		// Wait on primary/first chosen provider
		ctx := context.Background()
		if err := waitForIdle(ctx, chosen[0].StateDir, chosen[0].Running, opts, nil); err != nil {
			return err
		}
	}

	return cmdUpdate(rest, force, dryRun)
}

// cmdDockerIdleUpdate handles urnet-docker idle-update on the host.
func cmdDockerIdleUpdate(args []string, force, dryRun bool) error {
	opts, rest, err := parseIdleArgs(args)
	if err != nil {
		return err
	}

	providers := DiscoverDocker()
	t, _, err := updateTargetFromArgs(rest, providers)
	if err != nil {
		return err
	}

	if t.Unit == "" {
		switch len(providers) {
		case 0:
			return fmt.Errorf("no provider containers found")
		case 1:
			t.Unit = providers[0].Unit
		default:
			return fmt.Errorf("multiple provider containers found; specify which one with --unit")
		}
	}

	p, err := selectTarget(providers, t)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Printf("[dry-run] would wait for container %s to reach idle: threshold=%s, window=%s, timeout=%s\n",
			p.Unit, formatRate(opts.Threshold), opts.Window, opts.Timeout)
		return cmdDockerUpdate(rest, force, dryRun)
	}

	// Host-side poller executing into container to read /root/.urnetwork/billable_rate
	dockerPoll := func() (uint64, bool, error) {
		cmd := exec.Command(dockerCLI(), "exec", p.Unit, "cat", "/root/.urnetwork/billable_rate")
		out, err := cmd.Output()
		if err != nil {
			return 0, false, nil
		}
		s := strings.TrimSpace(string(out))
		val, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, false, nil
		}
		return val, true, nil
	}

	ctx := context.Background()
	if err := waitForIdle(ctx, "", p.Running, opts, dockerPoll); err != nil {
		return err
	}

	return cmdDockerUpdate(rest, force, dryRun)
}
