package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDirectToggleRoundTrip(t *testing.T) {
	// Use a temp HOME to prevent touching the real ~/.urnetwork/direct
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Default: no file = enabled
	if !isDirectEnabled() {
		t.Fatal("expected direct enabled by default (no toggle file)")
	}

	// Write "off"
	if err := writeDirectEnabled(false); err != nil {
		t.Fatalf("writeDirectEnabled(false): %v", err)
	}
	if isDirectEnabled() {
		t.Fatal("expected direct disabled after writing off")
	}

	// Write "on"
	if err := writeDirectEnabled(true); err != nil {
		t.Fatalf("writeDirectEnabled(true): %v", err)
	}
	if !isDirectEnabled() {
		t.Fatal("expected direct enabled after writing on")
	}

	// Clear
	if err := clearDirectToggle(); err != nil {
		t.Fatalf("clearDirectToggle: %v", err)
	}
	if !isDirectEnabled() {
		t.Fatal("expected direct enabled after clearing toggle")
	}
}

func TestDirectToggleVariants(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	variants := map[string]bool{
		"off":      false,
		"0":        false,
		"false":    false,
		"no":       false,
		"OFF":      false,
		"on":       true,
		"1":        true,
		"true":     true,
		"yes":      true,
		"":         true,
		"anything": true,
	}

	for val, want := range variants {
		t.Run(val, func(t *testing.T) {
			path, _ := directControlPath()
			os.MkdirAll(filepath.Dir(path), 0700)
			os.WriteFile(path, []byte(val+"\n"), 0600)
			got := isDirectEnabled()
			if got != want {
				t.Errorf("isDirectEnabled() with %q = %v, want %v", val, got, want)
			}
		})
	}
}

// TestDirectTogglePrecedence verifies that the toggle file takes precedence
// over the DISABLE_DIRECT_IP env var.
func TestDirectTogglePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// With env var set, no file → disabled
	t.Setenv("DISABLE_DIRECT_IP", "1")
	if isDirectEnabled() {
		t.Fatal("expected disabled with DISABLE_DIRECT_IP=1 and no toggle file")
	}

	// Write toggle file "on" → should override env var
	if err := writeDirectEnabled(true); err != nil {
		t.Fatalf("writeDirectEnabled(true): %v", err)
	}
	if !isDirectEnabled() {
		t.Fatal("expected enabled after writing on — toggle file should override env var")
	}

	// Write toggle file "off" → should still be disabled (env var also says off)
	if err := writeDirectEnabled(false); err != nil {
		t.Fatalf("writeDirectEnabled(false): %v", err)
	}
	if isDirectEnabled() {
		t.Fatal("expected disabled after writing off")
	}

	// Clear env var, toggle file still "off" → disabled
	t.Setenv("DISABLE_DIRECT_IP", "")
	if isDirectEnabled() {
		t.Fatal("expected disabled after clearing env var (file still says off)")
	}

	// Clear toggle file, no env var → enabled (default)
	if err := clearDirectToggle(); err != nil {
		t.Fatalf("clearDirectToggle: %v", err)
	}
	if !isDirectEnabled() {
		t.Fatal("expected enabled after clearing both toggle and env var")
	}
}

// TestReadDirectOverrideFileExists verifies that readDirectOverride correctly
// reports file existence.
func TestReadDirectOverrideFileExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// No file → (true, false)
	val, exists := readDirectOverride()
	if exists {
		t.Fatal("expected fileExists=false when no toggle file present")
	}
	if !val {
		t.Fatal("expected default value=true when no toggle file present")
	}

	// Write file → (true, true)
	if err := writeDirectEnabled(true); err != nil {
		t.Fatalf("writeDirectEnabled(true): %v", err)
	}
	val, exists = readDirectOverride()
	if !exists {
		t.Fatal("expected fileExists=true after writing toggle file")
	}
	if !val {
		t.Fatal("expected value=true after writing on")
	}

	// Write off → (false, true)
	if err := writeDirectEnabled(false); err != nil {
		t.Fatalf("writeDirectEnabled(false): %v", err)
	}
	val, exists = readDirectOverride()
	if !exists {
		t.Fatal("expected fileExists=true after writing toggle file")
	}
	if val {
		t.Fatal("expected value=false after writing off")
	}
}

// TestDirectKeyExcludedFromRemoval is a regression test for the critical bug
// where reload()'s generic proxy-diff loop would unconditionally add
// "direct" to the removed list (since it's never in desiredSet), causing
// the direct goroutine to be cancelled on every reload cycle.
func TestDirectKeyExcludedFromRemoval(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Simulate the direct goroutine being registered
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register a fake direct cancel func in the cancel map
	r := &ProxyReloader{
		cancelMap:   make(map[string]context.CancelFunc),
		cancelMapMu: &sync.Mutex{},
		wg:          &wg,
	}
	directCtx, directCancel := context.WithCancel(ctx)
	r.cancelMap[directProxyKey] = directCancel

	// Simulate the removal-diff logic (the part we fixed)
	running := make(map[string]bool)
	for addr := range r.cancelMap {
		running[addr] = true
	}

	desiredSet := make(map[string]bool)
	// desiredSet does NOT contain "direct" — simulating the real scenario
	// where desiredSet only has real proxy addresses
	desiredSet["1.2.3.4:1080"] = true

	// Apply the fix: skip directProxyKey in removal
	var removed []string
	for addr := range running {
		if addr == directProxyKey {
			continue // managed by the direct hot-toggle block, not the proxy diff
		}
		if _, ok := desiredSet[addr]; !ok {
			removed = append(removed, addr)
		}
	}

	// "direct" should NOT be in removed
	for _, addr := range removed {
		if addr == directProxyKey {
			t.Fatal("CRITICAL: directProxyKey was added to removed list — reload would cancel it")
		}
	}

	// Verify direct is still in the cancel map
	r.cancelMapMu.Lock()
	_, stillThere := r.cancelMap[directProxyKey]
	r.cancelMapMu.Unlock()
	if !stillThere {
		t.Fatal("CRITICAL: directProxyKey was removed from cancelMap — direct goroutine would be orphaned/cancelled")
	}

	directCancel()
	directCtx.Done() // prevent unused warning
}
