package urnettools

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetOverridesCache clears the in-process cache so each test starts fresh
// against its own temp $HOME instead of reusing another test's loaded state.
func resetOverridesCache() {
	overridesJSONFile.mu.Lock()
	overridesJSONFile.overrides = nil
	overridesJSONFile.loadedAt = time.Time{}
	overridesJSONFile.mu.Unlock()
}

// withTempHome points $HOME at a fresh temp dir and resets the override
// cache, so tests never see another test's overrides.json or cached state.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", "") // avoid a stray Windows-style fallback in CI
	resetOverridesCache()
	t.Cleanup(resetOverridesCache)
	return dir
}

func writeLegacyFile(t *testing.T, home, name, content string) {
	t.Helper()
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
}

func TestLoadOverridesJSON_MissingFile(t *testing.T) {
	withTempHome(t)

	ov, err := loadOverridesJSON()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ov != nil {
		t.Fatalf("expected nil overrides for missing file, got %+v", ov)
	}
}

func TestSaveThenLoadOverridesJSON_Roundtrip(t *testing.T) {
	withTempHome(t)

	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	resetOverridesCache() // force a real re-read from disk, not the write-through cache
	ov, err := loadOverridesJSON()
	if err != nil {
		t.Fatalf("loadOverridesJSON: %v", err)
	}
	if ov == nil {
		t.Fatalf("expected overrides after set, got nil")
	}
	if ov.NodeName != "nyc-1" {
		t.Fatalf("NodeName = %q, want %q", ov.NodeName, "nyc-1")
	}
}

func TestSaveOverridesJSON_AtomicAndScopedPerms(t *testing.T) {
	home := withTempHome(t)

	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	path := filepath.Join(home, ".urnetwork", "overrides.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat overrides.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("overrides.json perms = %o, want 0600", perm)
	}

	// no leftover temp files from the write-then-rename
	entries, err := os.ReadDir(filepath.Join(home, ".urnetwork"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "overrides.json" {
			t.Fatalf("unexpected leftover file in .urnetwork: %s", e.Name())
		}
	}
}

// TestGetOverrideValue_UnsetKeyFallsThroughToLegacy is the regression test
// for the bug fixed alongside this change: creating overrides.json to set
// ONE key must not blank out every other key that only exists in its legacy
// file. Before the fix, getOverrideValue returned ("", true) for any key
// overrides.json didn't happen to have set, discarding the legacy fallback.
func TestGetOverrideValue_UnsetKeyFallsThroughToLegacy(t *testing.T) {
	home := withTempHome(t)

	// A legacy file exists from before overrides.json was ever created.
	writeLegacyFile(t, home, "report_url", "https://old-report.example\n")

	// overrides.json now exists, but only because a DIFFERENT key was set.
	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	value, found := getOverrideValue("report_url", "report_url")
	if !found {
		t.Fatalf("report_url: found = false, want true (should fall back to legacy file)")
	}
	if value != "https://old-report.example" {
		t.Fatalf("report_url = %q, want legacy file contents", value)
	}
}

func TestGetOverrideValue_JSONValueWins(t *testing.T) {
	home := withTempHome(t)

	writeLegacyFile(t, home, "report_url", "https://old-report.example\n")
	if err := setKey("report_url", "https://new-report.example"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	value, found := getOverrideValue("report_url", "report_url")
	if !found || value != "https://new-report.example" {
		t.Fatalf("got (%q, %v), want (%q, true)", value, found, "https://new-report.example")
	}
}

func TestGetOverrideValue_NoJSONNoLegacy(t *testing.T) {
	withTempHome(t)

	value, found := getOverrideValue("node_name", "node_name")
	if found {
		t.Fatalf("expected not found, got (%q, true)", value)
	}
}

func TestGetOverrideValue_NoJSONReadsLegacy(t *testing.T) {
	home := withTempHome(t)
	writeLegacyFile(t, home, "node_name", "  legacy-node  \n")

	value, found := getOverrideValue("node_name", "node_name")
	if !found {
		t.Fatalf("expected found = true from legacy file")
	}
	if value != "legacy-node" {
		t.Fatalf("value = %q, want trimmed %q", value, "legacy-node")
	}
}

// TestGetOverrideBool_FalseFallsThroughToLegacy is the bool-side twin of the
// string-side regression test above: a JSON field sitting at its zero value
// (false) must not be trusted as "explicitly off" over a legacy marker file
// that says otherwise.
func TestGetOverrideBool_FalseFallsThroughToLegacy(t *testing.T) {
	home := withTempHome(t)

	// Legacy marker file present = "on" under the old scheme.
	writeLegacyFile(t, home, "fast_auth", "")

	// overrides.json now exists (for an unrelated key) but never set fast_auth.
	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	value, found := getOverrideBool("fast_auth", "fast_auth")
	if !found || !value {
		t.Fatalf("got (%v, %v), want (true, true) from legacy marker file", value, found)
	}
}

func TestGetOverrideBool_ExplicitTrueInJSON(t *testing.T) {
	withTempHome(t)

	if err := setKey("fast_auth", "on"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	value, found := getOverrideBool("fast_auth", "fast_auth")
	if !found || !value {
		t.Fatalf("got (%v, %v), want (true, true)", value, found)
	}
}

func TestGetOverrideBool_NoJSONNoLegacy(t *testing.T) {
	withTempHome(t)

	value, found := getOverrideBool("disable_ip_autodetect", "disable_ip_autodetect")
	if found || value {
		t.Fatalf("got (%v, %v), want (false, false)", value, found)
	}
}

func TestSetKey_ClearKeyRoundtrip(t *testing.T) {
	withTempHome(t)

	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}
	if value, found := getOverrideValue("node_name", "node_name"); !found || value != "nyc-1" {
		t.Fatalf("after set: got (%q, %v)", value, found)
	}

	if err := clearKey("node_name"); err != nil {
		t.Fatalf("clearKey: %v", err)
	}
	// Cleared with no legacy file backing it: falls all the way through.
	if value, found := getOverrideValue("node_name", "node_name"); found {
		t.Fatalf("after clear: got (%q, %v), want not found", value, found)
	}
}

func TestSetKey_BoolOffOn(t *testing.T) {
	withTempHome(t)

	if err := setKey("fast_auth", "on"); err != nil {
		t.Fatalf("setKey on: %v", err)
	}
	if value, _ := getOverrideBool("fast_auth", ""); !value {
		t.Fatalf("expected fast_auth true after 'on'")
	}

	if err := setKey("fast_auth", "off"); err != nil {
		t.Fatalf("setKey off: %v", err)
	}
	resetOverridesCache()
	ov, err := loadOverridesJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ov.FastAuth {
		t.Fatalf("expected fast_auth field false after 'off'")
	}
}

func TestSetKey_ProxyURLMaxParsing(t *testing.T) {
	withTempHome(t)

	if err := setKey("proxy_url_max", "42"); err != nil {
		t.Fatalf("setKey: %v", err)
	}
	resetOverridesCache()
	ov, err := loadOverridesJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ov.ProxyURLMax != 42 {
		t.Fatalf("ProxyURLMax = %d, want 42", ov.ProxyURLMax)
	}

	if err := setKey("proxy_url_max", "not-a-number"); err == nil {
		t.Fatalf("expected error for non-numeric proxy_url_max")
	}

	if err := setKey("proxy_url_max", "off"); err != nil {
		t.Fatalf("setKey off: %v", err)
	}
	resetOverridesCache()
	ov, err = loadOverridesJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ov.ProxyURLMax != 0 {
		t.Fatalf("ProxyURLMax after 'off' = %d, want 0", ov.ProxyURLMax)
	}
}

func TestSetKey_UnknownKeyErrors(t *testing.T) {
	withTempHome(t)

	if err := setKey("not-a-real-key", "value"); err == nil {
		t.Fatalf("expected error for unknown key")
	}
	if err := clearKey("not-a-real-key"); err == nil {
		t.Fatalf("expected error for unknown key on clear")
	}
}

func TestClearKey_NoOverridesJSONIsNoop(t *testing.T) {
	withTempHome(t)

	if err := clearKey("node_name"); err != nil {
		t.Fatalf("clearKey with no overrides.json should be a no-op, got: %v", err)
	}
	if hasOverridesJSON() {
		t.Fatalf("clearKey should not create overrides.json")
	}
}

func TestHasOverridesJSON(t *testing.T) {
	withTempHome(t)

	if hasOverridesJSON() {
		t.Fatalf("expected false before any write")
	}
	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}
	if !hasOverridesJSON() {
		t.Fatalf("expected true after write")
	}
}

// TestSetKey_ConcurrentDifferentKeys exercises the read-modify-write race the
// in-process mutex exists to prevent: many goroutines each set a DIFFERENT
// key at the same time. Without serializing the load-modify-save cycle,
// concurrent writers can stomp on each other's changes to other fields.
func TestSetKey_ConcurrentDifferentKeys(t *testing.T) {
	withTempHome(t)

	keys := []struct{ key, value string }{
		{"node_name", "nyc-1"},
		{"report_url", "https://report.example"},
		{"report_interval", "60s"},
		{"proxy_url_refresh", "5m"},
		{"cleanup_scope", "dead"},
		{"cleanup_interval", "1h"},
		{"fast_auth", "on"},
	}

	var wg sync.WaitGroup
	errs := make([]error, len(keys))
	for i, kv := range keys {
		wg.Add(1)
		go func(i int, key, value string) {
			defer wg.Done()
			errs[i] = setKey(key, value)
		}(i, kv.key, kv.value)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("setKey(%s) returned error: %v", keys[i].key, err)
		}
	}

	resetOverridesCache()
	ov, err := loadOverridesJSON()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ov == nil {
		t.Fatalf("expected overrides after concurrent writes")
	}

	got := map[string]string{
		"node_name":         ov.NodeName,
		"report_url":        ov.ReportURL,
		"report_interval":   ov.ReportInterval,
		"proxy_url_refresh": ov.ProxyURLRefresh,
		"cleanup_scope":     ov.CleanupScope,
		"cleanup_interval":  ov.CleanupInterval,
	}
	want := map[string]string{
		"node_name":         "nyc-1",
		"report_url":        "https://report.example",
		"report_interval":   "60s",
		"proxy_url_refresh": "5m",
		"cleanup_scope":     "dead",
		"cleanup_interval":  "1h",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field for %s = %q, want %q (a concurrent write stomped on it)", k, got[k], v)
		}
	}
	if !ov.FastAuth {
		t.Errorf("FastAuth = false, want true (a concurrent write stomped on it)")
	}
}

func TestGetOverrideValue_InProcessCache(t *testing.T) {
	home := withTempHome(t)

	if err := setKey("node_name", "nyc-1"); err != nil {
		t.Fatalf("setKey: %v", err)
	}

	// Overwrite the file on disk directly, bypassing setKey/the cache.
	path := filepath.Join(home, ".urnetwork", "overrides.json")
	if err := os.WriteFile(path, []byte(`{"node_name":"direct-write"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Within the cache window, the stale in-process value should still win.
	value, _ := getOverrideValue("node_name", "node_name")
	if value != "nyc-1" {
		t.Fatalf("expected cached value %q, got %q", "nyc-1", value)
	}

	// After the cache is invalidated, the on-disk value should be seen.
	resetOverridesCache()
	value, _ = getOverrideValue("node_name", "node_name")
	if value != "direct-write" {
		t.Fatalf("expected fresh value %q, got %q", "direct-write", value)
	}
}
