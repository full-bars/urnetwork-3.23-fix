package urnettools

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// overridesPath returns ~/.urnetwork/overrides.json
func overridesPath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	if home == "" {
		return "", fmt.Errorf("cannot resolve overrides path: $HOME is not set")
	}
	return filepath.Join(home, ".urnetwork", "overrides.json"), nil
}

// Overrides is the unified runtime tuning file. All keys are optional —
// absent keys mean "use the startup default" (same as a cleared legacy file).
type Overrides struct {
	NodeName            string `json:"node_name,omitempty"`
	ReportURL           string `json:"report_url,omitempty"`
	ReportInterval      string `json:"report_interval,omitempty"`
	DisableIPAutodetect bool   `json:"disable_ip_autodetect,omitempty"`
	FastAuth            bool   `json:"fast_auth,omitempty"`
	ProxyURLMax         int    `json:"proxy_url_max,omitempty"`
	ProxyURLRefresh     string `json:"proxy_url_refresh,omitempty"`
	CleanupScope        string `json:"cleanup_scope,omitempty"`
	CleanupInterval     string `json:"cleanup_interval,omitempty"`

	// Source tracks which path the values came from, for diagnostics.
	// "json" = overrides.json, "legacy" = individual files, "" = defaults.
	Source string `json:"-"`
}

// overridesJSONFile is the lock-protected in-process cache of overrides.json.
// All reads and writes go through this to prevent read-modify-write races
// when concurrent `set` calls target different keys in the same JSON file.
var overridesJSONFile struct {
	mu        sync.Mutex
	overrides *Overrides
	loadedAt  time.Time
}

// loadOverridesJSON reads and parses overrides.json with a flock to prevent
// concurrent writers from corrupting the file. Returns nil if the file doesn't
// exist (caller should fall back to legacy files).
func loadOverridesJSON() (*Overrides, error) {
	overridesJSONFile.mu.Lock()
	defer overridesJSONFile.mu.Unlock()
	return loadOverridesJSONLocked()
}

// loadOverridesJSONLocked is loadOverridesJSON's body, split out so setKey
// and clearKey can hold overridesJSONFile.mu across their whole
// load-modify-save sequence instead of only across each half. Locking each
// half separately (load, then later save) still lets two concurrent setKey
// calls both load the same base struct, each change a different field on
// their own copy, and save one after the other — the second save silently
// discards the first save's change. Callers MUST hold overridesJSONFile.mu
// before calling this.
func loadOverridesJSONLocked() (*Overrides, error) {
	path, err := overridesPath()
	if err != nil {
		return nil, err
	}

	// In-process cache: don't re-read if we loaded within the last 5s.
	// The 60s provider ticker is the real consumer; this just prevents
	// multiple concurrent calls in the same process.
	if overridesJSONFile.overrides != nil && time.Since(overridesJSONFile.loadedAt) < 5*time.Second {
		return overridesJSONFile.overrides, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // file doesn't exist — caller falls back to legacy
		}
		return nil, err
	}
	defer f.Close()

	// Exclusive lock for the read — prevents a concurrent writer from
	// tearing the file mid-parse. Uses flock(2) on Unix; no-op on Windows
	// where flock is unavailable (the in-process mutex + atomic rename
	// provide the same protection for single-process providers).
	if err := lockShared(f.Fd()); err != nil {
		return nil, fmt.Errorf("failed to lock overrides.json: %w", err)
	}
	defer unlock(f.Fd())

	var ov Overrides
	dec := json.NewDecoder(f)
	if err := dec.Decode(&ov); err != nil {
		if err == io.EOF {
			// Empty file = no overrides (equivalent to not having the file)
			ov = Overrides{}
		} else {
			return nil, fmt.Errorf("overrides.json: %w", err)
		}
	}
	ov.Source = "json"
	overridesJSONFile.overrides = &ov
	overridesJSONFile.loadedAt = time.Now()
	return &ov, nil
}

// saveOverridesJSON writes the Overrides struct to overrides.json atomically:
// temp file → flock(LOCK_EX) → write → rename. The temp file and rename
// ensure the file is never partially written (matching the hub_ca.pem
// pattern at bandwidth_reporter.go:906).
func saveOverridesJSON(ov *Overrides) error {
	overridesJSONFile.mu.Lock()
	defer overridesJSONFile.mu.Unlock()
	return saveOverridesJSONLocked(ov)
}

// saveOverridesJSONLocked is saveOverridesJSON's body. Callers MUST hold
// overridesJSONFile.mu before calling this — see loadOverridesJSONLocked.
func saveOverridesJSONLocked(ov *Overrides) error {
	path, err := overridesPath()
	if err != nil {
		return err
	}

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// Write to a temp file in the same directory (so os.Rename is atomic)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".overrides.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // clean up on any failure

	// Exclusive lock while writing
	if err := lockExclusive(tmp.Fd()); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to lock temp file: %w", err)
	}
	defer unlock(tmp.Fd())

	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		tmp.Close()
		return err
	}

	// Write with trailing newline
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write([]byte("\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Preserve permissions
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}

	// Atomic rename
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	// Update in-process cache (caller already holds overridesJSONFile.mu)
	overridesJSONFile.overrides = ov
	overridesJSONFile.loadedAt = time.Now()

	return nil
}

// hasOverridesJSON returns true if overrides.json exists on disk.
func hasOverridesJSON() bool {
	path, err := overridesPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// stringField reads a string key's raw value straight off the struct. Used
// only to check presence (non-zero) before deciding whether JSON satisfies a
// read — see the comment on getOverrideValue for why this matters.
func (o *Overrides) stringField(key string) (string, bool) {
	switch key {
	case "node-name", "node_name":
		return o.NodeName, o.NodeName != ""
	case "report-url", "report_url":
		return o.ReportURL, o.ReportURL != ""
	case "report-interval", "report_interval":
		return o.ReportInterval, o.ReportInterval != ""
	case "proxy-url-refresh", "proxy_url_refresh":
		return o.ProxyURLRefresh, o.ProxyURLRefresh != ""
	case "cleanup-scope", "cleanup_scope":
		return o.CleanupScope, o.CleanupScope != ""
	case "cleanup-interval", "cleanup_interval":
		return o.CleanupInterval, o.CleanupInterval != ""
	}
	return "", false
}

// setKey sets a key on the Overrides struct and saves atomically.
// This is the single entry point for ALL override writes, ensuring
// the JSON is always consistent.
func setKey(key, value string) error {
	overridesJSONFile.mu.Lock()
	defer overridesJSONFile.mu.Unlock()

	// Load existing (or start fresh). Holding the lock for the whole
	// load-modify-save cycle is what actually prevents two concurrent
	// setKey calls on different keys from stomping on each other.
	ov, err := loadOverridesJSONLocked()
	if err != nil {
		return err
	}
	if ov == nil {
		ov = &Overrides{}
	}

	switch key {
	case "node-name", "node_name":
		ov.NodeName = value
	case "report-url", "report_url":
		ov.ReportURL = value
	case "report-interval", "report_interval":
		ov.ReportInterval = value
	case "disable-ip-autodetect", "disable_ip_autodetect":
		if strings.ToLower(value) == "off" {
			ov.DisableIPAutodetect = false
		} else {
			ov.DisableIPAutodetect = true
		}
	case "fast-auth", "fast_auth":
		if strings.ToLower(value) == "off" {
			ov.FastAuth = false
		} else {
			ov.FastAuth = true
		}
	case "proxy-url-max", "proxy_url_max":
		if value == "off" || value == "" {
			ov.ProxyURLMax = 0
		} else {
			n, err := fmt.Sscanf(value, "%d", &ov.ProxyURLMax)
			if err != nil || n != 1 {
				return fmt.Errorf("proxy-url-max: invalid integer %q", value)
			}
		}
	case "proxy-url-refresh", "proxy_url_refresh":
		ov.ProxyURLRefresh = value
	case "cleanup-scope", "cleanup_scope":
		ov.CleanupScope = value
	case "cleanup-interval", "cleanup_interval":
		ov.CleanupInterval = value
	default:
		return fmt.Errorf("unknown override key %q", key)
	}

	return saveOverridesJSONLocked(ov)
}

// clearKey removes a key from the Overrides struct (clears the JSON field).
func clearKey(key string) error {
	overridesJSONFile.mu.Lock()
	defer overridesJSONFile.mu.Unlock()

	ov, err := loadOverridesJSONLocked()
	if err != nil {
		return err
	}
	// No overrides.json yet: still validate the key below so a typo'd key
	// errors out instead of silently succeeding. Only skip the write itself
	// (nothing to persist, and clearing shouldn't be what creates the file).
	creatingNothing := ov == nil
	if creatingNothing {
		ov = &Overrides{}
	}

	switch key {
	case "node-name", "node_name":
		ov.NodeName = ""
	case "report-url", "report_url":
		ov.ReportURL = ""
	case "report-interval", "report_interval":
		ov.ReportInterval = ""
	case "disable-ip-autodetect", "disable_ip_autodetect":
		ov.DisableIPAutodetect = false
	case "fast-auth", "fast_auth":
		ov.FastAuth = false
	case "proxy-url-max", "proxy_url_max":
		ov.ProxyURLMax = 0
	case "proxy-url-refresh", "proxy_url_refresh":
		ov.ProxyURLRefresh = ""
	case "cleanup-scope", "cleanup_scope":
		ov.CleanupScope = ""
	case "cleanup-interval", "cleanup_interval":
		ov.CleanupInterval = ""
	default:
		return fmt.Errorf("unknown override key %q", key)
	}

	if creatingNothing {
		return nil
	}
	return saveOverridesJSONLocked(ov)
}

// getOverrideValue reads a single key from overrides.json (or legacy
// fallback). A key that overrides.json has never had set for it (the zero
// value) is NOT treated as "found empty" — it falls through to the legacy
// file exactly as if overrides.json didn't exist, so creating the JSON file
// to set one key can never blank out every other still-unmigrated key.
func getOverrideValue(key, legacyFile string) (string, bool) {
	ov, err := loadOverridesJSON()
	if err == nil && ov != nil {
		if value, present := ov.stringField(key); present {
			return value, true
		}
	}

	// Fall back to legacy file
	if legacyFile == "" {
		return "", false
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	if home == "" {
		return "", false
	}
	path := filepath.Join(home, ".urnetwork", legacyFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// getOverrideBool reads a boolean key from overrides.json (or legacy marker
// file). Only an explicit `true` in JSON counts as "found" — a `false` field
// is indistinguishable from "never set" for a bool, so (like
// getOverrideValue) it falls through to the legacy marker file instead of
// assuming JSON already answered the question.
func getOverrideBool(key, legacyFile string) (bool, bool) {
	ov, err := loadOverridesJSON()
	if err == nil && ov != nil {
		switch key {
		case "disable-ip-autodetect", "disable_ip_autodetect":
			if ov.DisableIPAutodetect {
				return true, true
			}
		case "fast-auth", "fast_auth":
			if ov.FastAuth {
				return true, true
			}
		}
	}

	// Fall back to legacy marker file (presence = true)
	if legacyFile == "" {
		return false, false
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	if home == "" {
		return false, false
	}
	path := filepath.Join(home, ".urnetwork", legacyFile)
	_, err = os.Stat(path)
	return err == nil, err == nil || !os.IsNotExist(err)
}
