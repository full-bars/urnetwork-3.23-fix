package main

import (
	"os"
	"path/filepath"
	"testing"
)

// clearProfileRamlogsEnv resets the two env vars seedEnvFromControlState
// writes, and restores them after the test via t.Cleanup — otherwise a
// value written by one test's seed call would leak into whichever test
// happens to run next in the same binary.
func clearProfileRamlogsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_PROFILE", "")
	t.Setenv("URNETWORK_RAMLOGS", "")
}

func TestSeedEnvFromControlState_NoFilesIsNoop(t *testing.T) {
	withTempHome(t)
	clearProfileRamlogsEnv(t)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "" {
		t.Errorf("URNETWORK_PROFILE = %q, want unset", v)
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want unset", v)
	}
}

func TestSeedEnvFromControlState_ReadsPersistedProfile(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "provider_state.json"),
		`{"profile":"lowmem","ramlogs":"on"}`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "lowmem" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "lowmem")
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "1" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "1")
	}
}

func TestSeedEnvFromControlState_RamlogsOffSetsZero(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "provider_state.json"),
		`{"ramlogs":"off"}`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "0" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "0")
	}
}

// TestSeedEnvFromControlState_PendingQueueTakesEffectOnFirstStart is the
// exact scenario PR X exists for: a fresh install where `urnet-tools set
// profile ...` was queued in pending_overrides.json because the provider
// wasn't running yet (no provider_state.json exists at all). The very
// first startup must still see it.
func TestSeedEnvFromControlState_PendingQueueTakesEffectOnFirstStart(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"turbo-v4"},{"op":"set","key":"ramlogs","value":"on"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "turbo-v4" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "turbo-v4")
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "1" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "1")
	}

	// A read-only peek must not touch the queue file itself — that stays
	// mergePendingOverrides()'s job, inside provide().
	if _, err := os.Stat(filepath.Join(dir, ".urnetwork", "pending_overrides.json")); err != nil {
		t.Errorf("pending_overrides.json should be untouched by the peek, stat error: %v", err)
	}
}

// TestSeedEnvFromControlState_PendingQueueOverridesPersisted mirrors
// mergePendingOverrides()'s own precedence: a queued change on top of an
// already-persisted value wins, same order the real merge would apply.
func TestSeedEnvFromControlState_PendingQueueOverridesPersisted(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "provider_state.json"),
		`{"profile":"eco"}`)
	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"auto"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "auto" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q (pending queue should win over persisted state)", v, "auto")
	}
}

func TestSeedEnvFromControlState_MalformedFilesAreIgnored(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "provider_state.json"), `not json`)
	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "pending_overrides.json"), `also not json`)

	// Must not panic, and must leave the env vars alone.
	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "" {
		t.Errorf("URNETWORK_PROFILE = %q, want unset on malformed input", v)
	}
}

// TestSeedEnvFromControlState_NullPersistedStateThenPendingSetDoesNotPanic
// covers a decode edge case distinct from "malformed JSON": a literal JSON
// `null` unmarshals successfully into a nil map (no error), so a naive
// unconditional `values = onDisk` assignment replaces the initialized
// non-nil `values` with nil. The pending-queue overlay right after this
// then panics on its first `values[op.Key] = op.Value` write into that nil
// map. Regression test for that exact sequence: null persisted state,
// followed by a queued set.
func TestSeedEnvFromControlState_NullPersistedStateThenPendingSetDoesNotPanic(t *testing.T) {
	dir := withTempHome(t)
	clearProfileRamlogsEnv(t)

	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "provider_state.json"), `null`)
	writeJSONFile(t, filepath.Join(dir, ".urnetwork", "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"turbo-v4"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "turbo-v4" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "turbo-v4")
	}
}

func writeJSONFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
