package urnettools

import (
	"bufio"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildReexecSelectionFlags(t *testing.T) {
	a := Provider{Unit: "A.service", StateDir: "/state/a"}
	b := Provider{Unit: "B.service", StateDir: "/state/b"}
	cases := []struct {
		name   string
		args   []string
		chosen []Provider
		want   []string
	}{
		{"all", []string{"tool", "update", "--all", "-f"}, []Provider{a, b}, []string{"tool", "update", "-f", "--tag", "v35.7", "--include", "A.service,B.service"}},
		{"unit and tag", []string{"tool", "update", "--unit", "old", "--tag", "v35.7", "--digest", "abc", "--url", "https://example.test/x"}, []Provider{b}, []string{"tool", "update", "--tag", "v35.7", "--digest", "abc", "--url", "https://example.test/x", "--unit", "B.service"}},
		{"spaces", []string{"tool", "-n", "update", "--state-dir=/old", "--user", "u", "--network=n", "--network-id", "id", "--include=x", "--exclude", "z"}, []Provider{{StateDir: "/state/with spaces"}}, []string{"tool", "-n", "update", "--tag", "v35.7", "--state-dir", "/state/with spaces"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildReexecArgs(tc.args, tc.chosen, false, "v35.7")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReexecSelectionRejectsAmbiguity(t *testing.T) {
	a := Provider{Unit: "same.service", StateDir: "/a"}
	b := Provider{Unit: "same.service", StateDir: "/b"}
	if err := validateReexecSelection([]Provider{a, b}, []Provider{a, b}); err == nil {
		t.Fatal("colliding labels accepted")
	}
	if target, err := reexecSingleTarget([]Provider{a, b}, a); err != nil || target.StateDir != "/a" {
		t.Fatalf("expected state directory fallback, got %+v, %v", target, err)
	}
	missing := []Provider{{User: "x"}}
	if err := validateReexecSelection(missing, missing); err == nil {
		t.Fatal("provider without a unit or state directory accepted")
	}
}

func TestUpdateReexecChangedSelectionRefuses(t *testing.T) {
	b := Provider{Unit: "B.service", StateDir: "/old", User: "u"}
	changed := Provider{Unit: "B.service", StateDir: "/new", User: "u"}
	old := updateDiscoverFn
	updateDiscoverFn = func() []Provider { return []Provider{changed} }
	t.Cleanup(func() { updateDiscoverFn = old })
	t.Setenv(urnetUpdateReexecEnv, "1")
	t.Setenv(urnetUpdatePinnedEnv, encodeReexecSelection([]Provider{b}))
	oldTTY, oldReader := stdinIsInteractiveOverride, stdinReader
	stdinIsInteractiveOverride = func() bool { return true }
	stdinReader = bufio.NewReader(strings.NewReader(""))
	t.Cleanup(func() { stdinIsInteractiveOverride, stdinReader = oldTTY, oldReader })
	err := cmdUpdate([]string{"--unit", "B.service", "--tag", "v35.7", "--digest", strings.Repeat("a", 64)}, false, true)
	if err == nil || !strings.Contains(err.Error(), "selection changed") || !strings.Contains(err.Error(), "nothing updated") {
		t.Fatalf("expected refusal before prompts or download, got %v", err)
	}
}

func TestUpdateReexecNeverReprompts(t *testing.T) {
	providers := []Provider{{Unit: "A.service"}, {Unit: "B.service"}}
	oldDiscover, oldPrivileged := updateDiscoverFn, isPrivileged
	oldTTY, oldReader := stdinIsInteractiveOverride, stdinReader
	updateDiscoverFn = func() []Provider { return providers }
	isPrivileged = func() bool { return true }
	stdinIsInteractiveOverride = func() bool { return true }
	stdinReader = bufio.NewReader(strings.NewReader("2\ny\nyes\n"))
	t.Cleanup(func() {
		updateDiscoverFn, isPrivileged = oldDiscover, oldPrivileged
		stdinIsInteractiveOverride, stdinReader = oldTTY, oldReader
	})
	t.Setenv(urnetUpdateReexecEnv, "1")
	t.Setenv(urnetUpdatePinnedEnv, encodeReexecSelection(providers[1:]))
	if err := cmdUpdate(nil, false, false); err == nil {
		t.Fatal("expected an unpinned child selection to be refused")
	}
	line, err := stdinReader.ReadString('\n')
	if err != nil || line != "2\n" {
		t.Fatalf("child consumed picker input: %q, %v", line, err)
	}
}

func TestUpdateToolFirstInteractiveExcludePinsChoice(t *testing.T) {
	t.Setenv(urnetUpdateReexecEnv, "")
	t.Setenv(urnetUpdatePinnedEnv, "")
	providers := []Provider{{Unit: "A.service", Binary: "/a"}, {Unit: "B.service", Binary: "/b"}, {Unit: "C.service", Binary: "/c"}}
	oldDiscover := updateDiscoverFn
	updateDiscoverFn = func() []Provider { return providers }
	t.Cleanup(func() { updateDiscoverFn = oldDiscover })
	oldTTY, oldReader := stdinIsInteractiveOverride, stdinReader
	stdinIsInteractiveOverride = func() bool { return true }
	stdinReader = bufio.NewReader(strings.NewReader("2\ny\nyes\n"))
	t.Cleanup(func() { stdinIsInteractiveOverride, stdinReader = oldTTY, oldReader })
	asset, err := runningToolAssetName()
	if err != nil {
		t.Fatal(err)
	}
	latestReleaseMu.Lock()
	oldRelease, oldTime := cachedLatest, cachedLatestTime
	cachedLatest = &releaseInfo{Tag: "v3.23.0-fix.35.7", ProviderDigest: strings.Repeat("a", 64), Assets: []releaseAsset{{Name: asset, Digest: "sha256:" + strings.Repeat("b", 64)}}}
	cachedLatestTime = time.Now()
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest, cachedLatestTime = oldRelease, oldTime
		latestReleaseMu.Unlock()
	})
	oldStage, oldSelf, oldNewer, oldExec := stageToolForEscalationFn, selfUpdateToolFn, isToolNewerFn, reExecToolFn
	stageToolForEscalationFn = func(updateConfig) string { return "staged" }
	selfUpdateToolFn = func(updateConfig) error { return nil }
	isToolNewerFn = func(string, string) bool { return true }
	called := false
	reExecToolFn = func(_ string, args, env []string) error {
		called = true
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--exclude") || !strings.Contains(joined, "--unit B.service") {
			t.Errorf("wrong child argv %v", args)
		}
		if !containsArg(env, urnetUpdatePinnedEnv+"="+encodeReexecSelection(providers[1:2])) {
			t.Errorf("missing pinned identity: %v", env)
		}
		return nil
	}
	t.Cleanup(func() {
		stageToolForEscalationFn, selfUpdateToolFn, isToolNewerFn, reExecToolFn = oldStage, oldSelf, oldNewer, oldExec
	})
	oldArgs := os.Args
	os.Args = []string{"urnet-tools", "update", "--exclude", "C.service"}
	t.Cleanup(func() { os.Args = oldArgs })
	if err := cmdUpdate([]string{"--exclude", "C.service"}, false, false); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("re-exec not called")
	}
}
