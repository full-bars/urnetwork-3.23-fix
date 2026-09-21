package urnettools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubStatus swaps the discovery and snapshot seams and isolates the
// persisted default provider (read from HOME) from the developer's box.
func stubStatus(t *testing.T, providers []Provider, snaps map[string]json.RawMessage, privileged bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	origD, origF, origP, origR := discoverStatusFn, fetchSnapshotFn, isPrivileged, renderSystemctlStatus
	t.Cleanup(func() {
		discoverStatusFn, fetchSnapshotFn, isPrivileged, renderSystemctlStatus = origD, origF, origP, origR
	})
	// Never run the box's real systemctl. The stub fails like an unresolvable
	// unit so the classic table (with its "state-dir:" row) is what prints.
	renderSystemctlStatus = func(Provider) error { return errors.New("stubbed") }
	discoverStatusFn = func() []Provider { return providers }
	isPrivileged = func() bool { return privileged }
	fetchSnapshotFn = func(p Provider) (*NodeSnapshot, json.RawMessage, error) {
		raw, ok := snaps[p.Unit]
		if !ok {
			return nil, nil, errSnapshotUnavailable
		}
		var s NodeSnapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, err
		}
		return &s, raw, nil
	}
}

func fixtureRaw(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(livestatusFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// statusStateDirLabel is the state directory row label of this platform's
// existing status output (see the test below for why it differs).
func statusStateDirLabel() string { return statusStateDirLabelFor(runtime.GOOS) }

func statusStateDirLabelFor(goos string) string {
	if goos == "windows" || goos == "darwin" {
		return "state dir:"
	}
	return "state-dir:"
}

func TestStatusAppendsLiveBlockWhenProviderAnswers(t *testing.T) {
	p := Provider{User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"": fixtureRaw(t, "node_snapshot_v1.json")}, true)
	out := captureStdout(t, func() {
		if err := cmdStatus(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, statusStateDirLabel()) || !strings.Contains(out, "Live (provider v3.23.0-fix.32.1)") {
		t.Fatalf("want classic output then live block:\n%s", out)
	}
	if strings.Index(out, statusStateDirLabel()) > strings.Index(out, "Live (provider") {
		t.Fatalf("live block must come after the existing output:\n%s", out)
	}
}

func TestStatusSkipsLiveBlockSilentlyWhenUnreachable(t *testing.T) {
	p := Provider{User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, nil, true)
	out := captureStdout(t, func() {
		if err := cmdStatus(nil); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "Live (provider") || !strings.Contains(out, statusStateDirLabel()) {
		t.Fatalf("existing output must stand alone:\n%s", out)
	}
}

func TestStatusJSONPrintsRawSnapshot(t *testing.T) {
	raw := fixtureRaw(t, "node_snapshot_v1.json")
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"a.service": raw}, true)
	out := captureStdout(t, func() {
		if err := cmdStatus([]string{"--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
	}
	json.Unmarshal(raw, &want)
	if got["version"] != want["version"] || got["state"] != "flowing" {
		t.Fatalf("got %v", got)
	}
	if strings.Contains(out, statusStateDirLabel()) {
		t.Fatalf("--json must not print the classic output:\n%s", out)
	}
}

func TestStatusJSONErrorsWhenUnavailable(t *testing.T) {
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, nil, true)
	var err error
	out := captureStdout(t, func() { err = cmdStatus([]string{"--json"}) })
	if err == nil || !errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("err = %v; want a wrapped errSnapshotUnavailable", err)
	}
	if out != "" {
		t.Fatalf("stdout must stay empty on failure, got %q", out)
	}
}

func TestStatusManyProvidersNoTargetPrintsSummary(t *testing.T) {
	ps := []Provider{
		{Unit: "a.service", User: "ua", StateDir: t.TempDir(), Running: true},
		{Unit: "b.service", User: "ub", StateDir: t.TempDir(), Running: true, Version: "v3.22.0"},
	}
	stubStatus(t, ps, map[string]json.RawMessage{"a.service": fixtureRaw(t, "node_snapshot_v1.json")}, true)
	out := captureStdout(t, func() {
		if err := cmdStatus(nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"2 providers found", "a.service", "FLOWING", "b.service", "RUNNING", "v3.22.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, statusStateDirLabel()) {
		t.Errorf("summary must not print the detail view:\n%s", out)
	}
}

func TestStatusManyProvidersTargetShowsDetail(t *testing.T) {
	ps := []Provider{
		{Unit: "a.service", User: "ua", StateDir: t.TempDir(), Running: true},
		{Unit: "b.service", User: "ub", StateDir: t.TempDir(), Running: true},
	}
	stubStatus(t, ps, map[string]json.RawMessage{"b.service": fixtureRaw(t, "node_snapshot_v1_idle_minimal.json")}, true)
	out := captureStdout(t, func() {
		if err := cmdStatus([]string{"--unit", "b.service"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "b.service") || !strings.Contains(out, "why idle") || strings.Contains(out, "providers found") {
		t.Fatalf("want detail for b only:\n%s", out)
	}
}

// An unknown target still errors exactly as before; the summary only replaces
// the no-target refusal.
func TestStatusManyProvidersBadTargetStillErrors(t *testing.T) {
	ps := []Provider{
		{Unit: "a.service", User: "ua", StateDir: t.TempDir(), Running: true},
		{Unit: "b.service", User: "ub", StateDir: t.TempDir(), Running: true},
	}
	stubStatus(t, ps, nil, true)
	if err := cmdStatus([]string{"--unit", "nope.service"}); err == nil || !strings.Contains(err.Error(), "matches no provider") {
		t.Fatalf("err = %v", err)
	}
	// --json with several providers and no target still needs a target.
	if err := cmdStatus([]string{"--json"}); err == nil || !strings.Contains(err.Error(), "specify a target") {
		t.Fatalf("--json without target: err = %v", err)
	}
}

// The sudo re-exec for another user's provider must carry --json through to
// the elevated child.
func TestStatusJSONSurvivesCrossUserElevation(t *testing.T) {
	p := Provider{Unit: "a.service", User: "someone-else-entirely", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, nil, false)
	var got []string
	origE := elevateSelfFunc
	t.Cleanup(func() { elevateSelfFunc = origE })
	elevateSelfFunc = func(args []string) error { got = args; return nil }
	if err := cmdStatus([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "status --json" {
		t.Fatalf("elevated argv = %v", got)
	}
}

// The existing status output differs by platform: Linux prints a table with a
// "state-dir:" row, while Windows and macOS print a styled panel whose row is
// "state dir:". The live-block tests look for that row to tell "the existing
// output" from the live block, so they must ask which label this platform uses.
func TestStatusStateDirLabelMatchesWhatEachPlatformPrints(t *testing.T) {
	for goos, want := range map[string]string{"linux": "state-dir:", "windows": "state dir:", "darwin": "state dir:"} {
		if got := statusStateDirLabelFor(goos); got != want {
			t.Errorf("statusStateDirLabelFor(%q) = %q, want %q", goos, got, want)
		}
	}
	// The label promised for Windows and macOS must really be in the panel they render.
	panel := captureStdout(t, func() {
		renderStatusPanel(Provider{User: "u", StateDir: t.TempDir(), Running: true})
	})
	if !strings.Contains(panel, statusStateDirLabelFor("windows")) {
		t.Fatalf("the styled panel used on Windows and macOS has no %q row:\n%s", statusStateDirLabelFor("windows"), panel)
	}
}
