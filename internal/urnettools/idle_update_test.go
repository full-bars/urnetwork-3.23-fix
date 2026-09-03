package urnettools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseIdleArgs(t *testing.T) {
	// Defaults
	opts, rest, err := parseIdleArgs([]string{"--unit=urnetwork.service", "--tag=v3.23.0-fix.30.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 5120 {
		t.Errorf("expected default threshold 5120, got %d", opts.Threshold)
	}
	if opts.Window != 5*time.Minute {
		t.Errorf("expected default window 5m, got %v", opts.Window)
	}
	if opts.Timeout != 30*time.Minute {
		t.Errorf("expected default timeout 30m, got %v", opts.Timeout)
	}
	if len(rest) != 2 {
		t.Errorf("expected 2 remaining args, got %v", rest)
	}

	// Custom values
	opts, rest, err = parseIdleArgs([]string{
		"--threshold", "10240",
		"--window", "10s",
		"--timeout", "1m",
		"--tag", "v3.23.0-fix.30.9",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 10240 {
		t.Errorf("expected threshold 10240, got %d", opts.Threshold)
	}
	if opts.Window != 10*time.Second {
		t.Errorf("expected window 10s, got %v", opts.Window)
	}
	if opts.Timeout != 1*time.Minute {
		t.Errorf("expected timeout 1m, got %v", opts.Timeout)
	}
	if len(rest) != 2 {
		t.Errorf("expected 2 remaining args, got %v", rest)
	}

	// Equals syntax
	opts, rest, err = parseIdleArgs([]string{
		"--threshold=2048",
		"--window=60",
		"--timeout=300",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 2048 {
		t.Errorf("expected threshold 2048, got %d", opts.Threshold)
	}
	if opts.Window != 60*time.Second {
		t.Errorf("expected window 60s, got %v", opts.Window)
	}
	if opts.Timeout != 300*time.Second {
		t.Errorf("expected timeout 300s, got %v", opts.Timeout)
	}
}

func TestReadBillableRateFile(t *testing.T) {
	dir := t.TempDir()
	ratePath := filepath.Join(dir, "billable_rate")

	// Not found
	rate, found, err := readBillableRateFile(dir)
	if err != nil || found || rate != 0 {
		t.Fatalf("expected not found, got rate=%d, found=%v, err=%v", rate, found, err)
	}

	// Valid rate
	if err := os.WriteFile(ratePath, []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rate, found, err = readBillableRateFile(dir)
	if err != nil || !found || rate != 12345 {
		t.Fatalf("expected rate=12345, got rate=%d, found=%v, err=%v", rate, found, err)
	}

	// Malformed rate
	if err := os.WriteFile(ratePath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readBillableRateFile(dir)
	if err == nil {
		t.Fatalf("expected error on malformed rate file, got nil")
	}
}

func TestWaitForIdleImmediate(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{Window: 0}
	err := waitForIdle(ctx, "/tmp", true, opts, nil)
	if err != nil {
		t.Fatalf("expected immediate completion on window=0, got %v", err)
	}
}

func TestWaitForIdleStoppedProvider(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{Window: 1 * time.Second, Timeout: 5 * time.Second}
	// When provider is stopped and billable_rate is missing, traffic is 0
	pollFn := func() (uint64, bool, error) {
		return 0, false, nil
	}
	err := waitForIdle(ctx, "/tmp", false, opts, pollFn)
	if err != nil {
		t.Fatalf("expected completion for stopped provider, got %v", err)
	}
}

func TestWaitForIdleTimeout(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{
		Threshold: 1000,
		Window:    10 * time.Minute,
		Timeout:   100 * time.Millisecond,
	}
	pollFn := func() (uint64, bool, error) {
		return 50000, true, nil // Always above threshold
	}
	start := time.Now()
	err := waitForIdle(ctx, "/tmp", true, opts, pollFn)
	if err != nil {
		t.Fatalf("expected graceful timeout return, got %v", err)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatalf("returned prematurely before timeout")
	}
}

func TestIdleUpdateDryRun(t *testing.T) {
	err := Run([]string{"idle-update", "--dry-run", "--unit=urnetwork.service"})
	if err != nil && err.Error() != "no providers found on this box" {
		t.Fatalf("unexpected idle-update dry-run error: %v", err)
	}
}

func TestDockerIdleUpdateDryRun(t *testing.T) {
	err := RunDocker([]string{"idle-update", "--dry-run", "--unit=test-provider"})
	if err != nil && err.Error() != "no provider containers found" {
		t.Fatalf("unexpected urnet-docker idle-update dry-run error: %v", err)
	}
}
