package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestControlState_SetGetClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	if _, found := s.get("node_name"); found {
		t.Fatalf("expected not found before any set")
	}

	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("got (%q, %v), want (%q, true)", v, found, "nyc-1")
	}

	if err := s.clear("node_name"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, found := s.get("node_name"); found {
		t.Fatalf("expected not found after clear")
	}
}

func TestControlState_UnknownKeyRejected(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	if err := s.set("not-a-real-key", "value"); err == nil {
		t.Fatalf("expected error for unknown key on set")
	}
	if err := s.clear("not-a-real-key"); err == nil {
		t.Fatalf("expected error for unknown key on clear")
	}
}

func TestControlState_PersistAndLoad_Roundtrip(t *testing.T) {
	home := withTempHome(t)
	s := newControlState()

	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.set("fast_auth", "on"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	loaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	if v, found := loaded.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v)", v, found)
	}
	if v, found := loaded.get("fast_auth"); !found || v != "on" {
		t.Fatalf("fast_auth: got (%q, %v)", v, found)
	}

	path := filepath.Join(home, ".urnetwork", "provider_state.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat provider_state.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("provider_state.json perms = %o, want 0600", perm)
	}
}

func TestLoadControlState_MissingFileIsEmptyNotError(t *testing.T) {
	withTempHome(t)

	s, err := loadControlState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, found := s.get("node_name"); found {
		t.Fatalf("expected empty state for a fresh box with no provider_state.json")
	}
}

// TestLoadControlState_UnknownKeyIsDroppedNotFatal covers a downgrade: a
// newer provider binary wrote a key this one doesn't recognize. Startup
// must not fail over it — the provider is the only writer, so this means
// "rolled back to an older binary," not "the file is corrupt."
func TestLoadControlState_UnknownKeyIsDroppedNotFatal(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"node_name":"nyc-1","some_future_key":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "provider_state.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := loadControlState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v)", v, found)
	}
}

// TestControlState_ConcurrentSetClear exercises the mutex under -race: many
// goroutines setting/clearing different keys at once must never corrupt the
// map or race on it — there's no read-modify-write hazard here (unlike the
// file-based design this replaces) because every method holds the lock for
// its entire body, but the persisted file's snapshot must still always be
// self-consistent.
func TestControlState_ConcurrentSetClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	keys := []string{
		"node_name", "report_url", "report_interval", "fast_auth",
		"proxy_self_heal", "proxy_url_max", "proxy_url_refresh",
		"proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval",
	}

	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.set(key, "v")
				s.get(key)
				s.persist()
			}
			s.clear(key)
		}(k)
	}
	wg.Wait()

	for _, k := range keys {
		if _, found := s.get(k); found {
			t.Errorf("key %s: expected cleared, still found", k)
		}
	}
}

func TestControlState_ReplaceAll(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	if err := s.set("node_name", "stale"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.set("hot_restart", "on"); err != nil {
		t.Fatalf("set: %v", err)
	}

	s.replaceAll(map[string]string{"node_name": "fresh"})

	if v, found := s.get("node_name"); !found || v != "fresh" {
		t.Fatalf("node_name = (%q, %v), want (%q, true)", v, found, "fresh")
	}
	if _, found := s.get("hot_restart"); found {
		t.Fatalf("hot_restart should be gone after replaceAll dropped it, still found")
	}
}

// TestControlState_ReplaceAllRacesWithConcurrentGet is the regression test
// for the HotSwap-candidate-takeover race: a candidate reloading
// provider_state.json calls replaceAll on the SAME *controlState instance
// every other proxy goroutine already holds and calls get() on (e.g.
// hotRestartEnabled -> globalControlState.get("hot_restart")), concurrently.
// The bug this guards against was reassigning the globalControlState
// *pointer* itself instead of mutating in place — a data race the -race
// detector catches on the pointer variable, not on anything s.mu protects.
// Exercising replaceAll (mutate in place) alongside concurrent get() must
// stay -race clean.
func TestControlState_ReplaceAllRacesWithConcurrentGet(t *testing.T) {
	withTempHome(t)
	s := newControlState()
	if err := s.set("hot_restart", "on"); err != nil {
		t.Fatalf("set: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Simulate other proxy goroutines reading control state throughout the
	// takeover window, same as provideAuth -> hotRestartEnabled would.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.get("hot_restart")
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		s.replaceAll(map[string]string{"hot_restart": "on"})
	}
	close(stop)
	wg.Wait()
}

// resetGlobalControlStateForTest resets the package-global control state to a
// clean empty state WITHOUT reassigning the pointer. Rewriting
// `globalControlState = newControlState()` was the source of an intermittent
// test data race: a background goroutine from a prior test (e.g. a hotswap or
// framer test) that reads globalControlState directly via hotRestartEnabled()
// or a resolve* function could race with the later socket test's pointer write.
// This helper preserves the single stable *controlState instance (exactly like
// production's replaceAll — see control_state.go) and instead clears all keys
// under the same locks every get/set/persist already uses, so map access stays
// synchronized and any in-flight set/clear is serialized against the reset.
func resetGlobalControlStateForTest() {
	globalControlState.txMu.Lock()
	defer globalControlState.txMu.Unlock()
	globalControlState.replaceAll(map[string]string{})
}

func TestControlState_V2EnvelopeRoundTrip(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	setAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	envelope := controlStateEnvelope{
		Version: 2,
		Values: map[string]string{
			"node_name":  "nyc-1",
			"gomemlimit": "2048mib",
		},
		Meta: map[string]configMeta{
			"node_name":  {Source: SourceSocket, SetAt: setAt},
			"gomemlimit": {Source: SourceEnv, SetAt: setAt},
		},
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider_state.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}

	val, meta, ok := loaded.getWithMeta("node_name")
	if !ok || val != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v), want (%q, true)", val, ok, "nyc-1")
	}
	if meta.Source != SourceSocket {
		t.Errorf("node_name source = %q, want %q", meta.Source, SourceSocket)
	}
	if !meta.SetAt.Equal(setAt) {
		t.Errorf("node_name set_at = %v, want %v", meta.SetAt, setAt)
	}

	val, meta, ok = loaded.getWithMeta("gomemlimit")
	if !ok || val != "2048mib" {
		t.Fatalf("gomemlimit: got (%q, %v), want (%q, true)", val, ok, "2048mib")
	}
	if meta.Source != SourceEnv {
		t.Errorf("gomemlimit source = %q, want %q", meta.Source, SourceEnv)
	}
	if !meta.SetAt.Equal(setAt) {
		t.Errorf("gomemlimit set_at = %v, want %v", meta.SetAt, setAt)
	}
}

func TestControlState_LegacyBackwardCompat(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacyData := []byte(`{"node_name":"legacy-node","fast_auth":"on"}`)
	if err := os.WriteFile(filepath.Join(dir, "provider_state.json"), legacyData, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}

	val, found := loaded.get("node_name")
	if !found || val != "legacy-node" {
		t.Fatalf("node_name: got (%q, %v), want (%q, true)", val, found, "legacy-node")
	}

	val, meta, found := loaded.getWithMeta("node_name")
	if !found || val != "legacy-node" {
		t.Fatalf("getWithMeta(node_name): got (%q, %v)", val, found)
	}
	if meta.Source != SourceLegacy {
		t.Errorf("expected SourceLegacy, got %q", meta.Source)
	}
	if !meta.SetAt.IsZero() {
		t.Errorf("expected zero SetAt for legacy, got %v", meta.SetAt)
	}

	val, meta, found = loaded.getWithMeta("fast_auth")
	if !found || val != "on" {
		t.Fatalf("getWithMeta(fast_auth): got (%q, %v)", val, found)
	}
	if meta.Source != SourceLegacy {
		t.Errorf("fast_auth: expected SourceLegacy, got %q", meta.Source)
	}
}

func TestControlState_SetWithValue(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	s := newControlState()

	sources := []Source{SourceSocket, SourceEnv, SourcePending, SourceLegacy, SourceDefault}
	for _, src := range sources {
		key := "node_name"
		val := "node-" + string(src)
		before := time.Now().Add(-time.Second)
		if err := s.setWithValue(key, val, src); err != nil {
			t.Fatalf("setWithValue(%q, %q, %q): %v", key, val, src, err)
		}
		gotVal, gotMeta, ok := s.getWithMeta(key)
		if !ok || gotVal != val {
			t.Fatalf("getWithMeta: got (%q, %v), want (%q, true)", gotVal, ok, val)
		}
		if gotMeta.Source != src {
			t.Errorf("got source %q, want %q", gotMeta.Source, src)
		}
		if gotMeta.SetAt.Before(before) || gotMeta.SetAt.After(time.Now().Add(time.Second)) {
			t.Errorf("SetAt %v not within expected window around %v", gotMeta.SetAt, before)
		}
	}
}

func TestControlState_ClearWithValue(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	s := newControlState()

	if err := s.setWithValue("node_name", "nyc-1", SourceSocket); err != nil {
		t.Fatalf("setWithValue: %v", err)
	}
	if _, _, ok := s.getWithMeta("node_name"); !ok {
		t.Fatalf("expected node_name to be set")
	}

	if err := s.clearWithValue("node_name"); err != nil {
		t.Fatalf("clearWithValue: %v", err)
	}

	val, meta, ok := s.getWithMeta("node_name")
	if ok {
		t.Fatalf("expected not found after clearWithValue, got val=%q, ok=%v", val, ok)
	}
	if meta != (configMeta{}) {
		t.Fatalf("expected zero configMeta after clearWithValue, got %+v", meta)
	}

	if _, found := s.get("node_name"); found {
		t.Fatalf("expected s.get to return found=false after clearWithValue")
	}
}

func TestControlState_StatusSnapshot(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	s := newControlState()

	if err := s.setWithValue("node_name", "host-1", SourceSocket); err != nil {
		t.Fatalf("setWithValue: %v", err)
	}
	if err := s.setWithValue("gomemlimit", "1gib", SourceEnv); err != nil {
		t.Fatalf("setWithValue: %v", err)
	}
	if err := s.setWithValue("gogc", "50", SourcePending); err != nil {
		t.Fatalf("setWithValue: %v", err)
	}

	snap := s.statusSnapshot()
	if len(snap) != len(controlKeys) {
		t.Fatalf("statusSnapshot len = %d, want %d (all controlKeys)", len(snap), len(controlKeys))
	}

	entry, ok := snap["node_name"]
	if !ok || entry.Value != "host-1" || entry.Meta.Source != SourceSocket {
		t.Errorf("node_name: got %+v, ok=%v", entry, ok)
	}
	if entry.Meta.SetAt.IsZero() {
		t.Errorf("node_name: expected non-zero SetAt")
	}

	entry, ok = snap["gomemlimit"]
	if !ok || entry.Value != "1gib" || entry.Meta.Source != SourceEnv {
		t.Errorf("gomemlimit: got %+v, ok=%v", entry, ok)
	}

	entry, ok = snap["gogc"]
	if !ok || entry.Value != "50" || entry.Meta.Source != SourcePending {
		t.Errorf("gogc: got %+v, ok=%v", entry, ok)
	}

	// Unoverridden keys should appear with SourceDefault.
	entry, ok = snap["proxy_self_heal"]
	if !ok || entry.Meta.Source != SourceDefault {
		t.Errorf("proxy_self_heal: expected SourceDefault, got %+v, ok=%v", entry, ok)
	}
}

func TestControlState_UnknownKeysPreserved(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	setAt := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	initialEnvelope := controlStateEnvelope{
		Version: 2,
		Values: map[string]string{
			"node_name":         "nyc-1",
			"future_knob_alpha": "custom_val_alpha",
			"future_knob_beta":  "custom_val_beta",
		},
		Meta: map[string]configMeta{
			"node_name":         {Source: SourceSocket, SetAt: setAt},
			"future_knob_alpha": {Source: SourceSocket, SetAt: setAt},
		},
	}
	data, err := json.MarshalIndent(initialEnvelope, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	statePath := filepath.Join(dir, "provider_state.json")
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load with current binary
	s, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}

	// Known key is recognized and accessible
	if v, found := s.get("node_name"); !found || v != "nyc-1" {
		t.Fatalf("node_name: got (%q, %v)", v, found)
	}

	// Unknown keys should NOT be exposed through get()
	if _, found := s.get("future_knob_alpha"); found {
		t.Fatalf("future_knob_alpha should not be exposed via get()")
	}

	// Mutate a known key and persist to trigger write-back
	if err := s.set("node_name", "nyc-2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Re-read file directly and verify unknown keys survived round-trip
	persistedBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	var out controlStateEnvelope
	if err := json.Unmarshal(persistedBytes, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Version != 2 {
		t.Errorf("version = %d, want 2", out.Version)
	}
	if out.Values["node_name"] != "nyc-2" {
		t.Errorf("node_name = %q, want %q", out.Values["node_name"], "nyc-2")
	}
	if out.Values["future_knob_alpha"] != "custom_val_alpha" {
		t.Errorf("future_knob_alpha value lost: got %q, want %q", out.Values["future_knob_alpha"], "custom_val_alpha")
	}
	if out.Values["future_knob_beta"] != "custom_val_beta" {
		t.Errorf("future_knob_beta value lost: got %q, want %q", out.Values["future_knob_beta"], "custom_val_beta")
	}
	if meta, ok := out.Meta["future_knob_alpha"]; !ok || meta.Source != SourceSocket {
		t.Errorf("future_knob_alpha meta lost or corrupted: %+v", meta)
	}
}

func TestControlState_PersistV2Format(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()
	s := newControlState()

	if err := s.setWithValue("node_name", "nyc-1", SourceSocket); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.setWithValue("fast_auth", "on", SourceEnv); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	path := filepath.Join(home, ".urnetwork", "provider_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}

	var envelope controlStateEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal into controlStateEnvelope: %v", err)
	}

	if envelope.Version != 2 {
		t.Errorf("version = %d, want 2", envelope.Version)
	}
	if envelope.Values["node_name"] != "nyc-1" {
		t.Errorf("values[node_name] = %q, want nyc-1", envelope.Values["node_name"])
	}
	if envelope.Values["fast_auth"] != "on" {
		t.Errorf("values[fast_auth] = %q, want on", envelope.Values["fast_auth"])
	}
	if meta, ok := envelope.Meta["node_name"]; !ok || meta.Source != SourceSocket || meta.SetAt.IsZero() {
		t.Errorf("meta[node_name] = %+v", meta)
	}
	if meta, ok := envelope.Meta["fast_auth"]; !ok || meta.Source != SourceEnv || meta.SetAt.IsZero() {
		t.Errorf("meta[fast_auth] = %+v", meta)
	}
}
