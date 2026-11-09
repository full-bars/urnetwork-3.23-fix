package urnettools

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompareVersionTags(t *testing.T) {
	cases := []struct {
		v1, v2 string
		want   int
	}{
		{"v3.23.0-fix.35.6", "v3.23.0-fix.35.5", 1},
		{"v3.23.0-fix.35.5", "v3.23.0-fix.35.6", -1},
		{"v3.23.0-fix.35.6", "v3.23.0-fix.35.6", 0},
		{"v3.23.0-fix.36.0", "v3.23.0-fix.35.6", 1},
		{"v3.24.0", "v3.23.0-fix.35.6", 1},
		{"v3.22.0", "v3.23.0-fix.35.6", -1},
		{"3.23.0-fix.35.6", "v3.23.0-fix.35.5", 1},
		{"v3.23.0-fix.35.6.1", "v3.23.0-fix.35.6", 1},
	}
	for _, tc := range cases {
		got := compareVersionTags(tc.v1, tc.v2)
		if got != tc.want {
			t.Errorf("compareVersionTags(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
		}
	}
}

func TestIsToolNewer(t *testing.T) {
	if !isToolNewer("v3.23.0-fix.35.6", "v3.23.0-fix.35.5") {
		t.Error("expected 35.6 to be newer than 35.5")
	}
	if isToolNewer("v3.23.0-fix.35.5", "v3.23.0-fix.35.6") {
		t.Error("expected 35.5 not to be newer than 35.6")
	}
	if isToolNewer("v3.23.0-fix.35.6", "v3.23.0-fix.35.6") {
		t.Error("expected equal versions not to be newer")
	}
	if isToolNewer("v3.23.0-fix.35.6", "dev") {
		t.Error("expected dev not to be superseded by release by default")
	}
	if !isToolNewer("v3.23.0-fix.35.6", "") {
		t.Error("expected release to be newer than empty string")
	}
}

func TestBuildReexecArgs(t *testing.T) {
	chosen := []Provider{
		{Unit: "urnetwork.service", Binary: "/usr/bin/provider"},
	}

	// Case 1: bare interactive arguments -> adds --tag and --unit
	args1 := buildReexecArgs([]string{"urnet-tools", "update"}, chosen, false, "v3.23.0-fix.35.6")
	joined1 := strings.Join(args1, " ")
	if !strings.Contains(joined1, "--tag v3.23.0-fix.35.6") || !strings.Contains(joined1, "--unit urnetwork.service") {
		t.Errorf("expected tag and unit added, got: %v", args1)
	}

	// Case 2: existing --tag and --all preserved without duplication
	args2 := buildReexecArgs([]string{"urnet-tools", "update", "--tag", "v3.23.0-fix.35.6", "--all"}, chosen, true, "v3.23.0-fix.35.6")
	if strings.Count(strings.Join(args2, " "), "--tag") != 1 || strings.Count(strings.Join(args2, " "), "--all") != 1 {
		t.Errorf("expected no duplication of --tag or --all, got: %v", args2)
	}

	// Case 3: multi-target picks -> adds --include
	multi := []Provider{
		{Unit: "u1.service", Binary: "/bin/p1"},
		{Unit: "u2.service", Binary: "/bin/p2"},
	}
	args3 := buildReexecArgs([]string{"urnet-tools", "update"}, multi, false, "v3.23.0-fix.35.6")
	joined3 := strings.Join(args3, " ")
	if !strings.Contains(joined3, "--include u1.service,u2.service") {
		t.Errorf("expected --include with labels, got: %v", args3)
	}
}

func TestUnattendedUpdate_ReexecEnv(t *testing.T) {
	t.Setenv(urnetUpdateReexecEnv, "1")
	if !unattendedUpdate(false) {
		t.Error("expected unattendedUpdate=true when URNET_TOOLS_UPDATE_REEXEC=1")
	}
}

func TestUpdate_ToolFirstOrdering(t *testing.T) {
	tempDir := t.TempDir()
	toolPath := filepath.Join(tempDir, "urnet-tools")
	if runtime.GOOS == "windows" {
		toolPath += ".exe"
	}
	oldToolBytes := fakeELF("tool-v35.5")
	if err := os.WriteFile(toolPath, oldToolBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	newToolBytes := fakeELF("tool-v35.6")
	sum := sha256.Sum256(newToolBytes)
	newToolDigest := hex.EncodeToString(sum[:])

	assetName, err := runningToolAssetName()
	if err != nil {
		t.Fatal(err)
	}

	tag := "v3.23.0-fix.35.6"
	fakeRel := &releaseInfo{
		Tag:            tag,
		URL:            "http://127.0.0.1:9/provider.tar.gz",
		ProviderDigest: strings.Repeat("c", 64),
		Assets: []releaseAsset{
			{Name: assetName, Digest: "sha256:" + newToolDigest},
		},
	}

	latestReleaseMu.Lock()
	origCached := cachedLatest
	origTime := cachedLatestTime
	cachedLatest = fakeRel
	cachedLatestTime = time.Now()
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest = origCached
		cachedLatestTime = origTime
		latestReleaseMu.Unlock()
	})

	origToolVersion := ToolVersion
	ToolVersion = "v3.23.0-fix.35.5"
	t.Cleanup(func() { ToolVersion = origToolVersion })

	origDisc := verifyDiscoverFn
	p := Provider{Unit: "urnetwork.service", Binary: filepath.Join(tempDir, "provider"), StateDir: tempDir}
	verifyDiscoverFn = func() []Provider { return []Provider{p} }
	t.Cleanup(func() { verifyDiscoverFn = origDisc })

	var reExecCalled bool
	var reExecExe string
	var reExecArgs []string
	var reExecEnv []string
	origReExec := reExecToolFn
	reExecToolFn = func(exe string, args []string, env []string) error {
		reExecCalled = true
		reExecExe = exe
		reExecArgs = args
		reExecEnv = env
		return nil
	}
	t.Cleanup(func() { reExecToolFn = origReExec })

	origStage := stageToolForEscalationFn
	stageToolForEscalationFn = func(cfg updateConfig) string {
		sp := filepath.Join(cfg.StageDir, cfg.ToolAsset)
		_ = os.WriteFile(sp, newToolBytes, 0o755)
		return sp
	}
	t.Cleanup(func() { stageToolForEscalationFn = origStage })

	origSelfUpdate := selfUpdateToolFn
	selfUpdateToolFn = func(cfg updateConfig) error {
		return selfUpdateToolTo(toolPath, cfg)
	}
	t.Cleanup(func() { selfUpdateToolFn = origSelfUpdate })

	// Run cmdUpdate with -f (unattended) and explicit --unit
	err = cmdUpdate([]string{"--unit", "urnetwork.service"}, true, false)
	if err != nil {
		t.Fatalf("cmdUpdate failed: %v", err)
	}

	if !reExecCalled {
		t.Fatal("expected tool-first update to re-exec the new tool")
	}
	_ = reExecExe

	// Verify the tool binary on disk was updated to the new tool bytes
	updatedBytes, err := os.ReadFile(toolPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedBytes) != string(newToolBytes) {
		t.Errorf("tool binary on disk was not updated before re-exec")
	}

	// Verify loop guard was passed in reExecEnv
	hasEnv := false
	for _, env := range reExecEnv {
		if env == urnetUpdateReexecEnv+"=1" {
			hasEnv = true
			break
		}
	}
	if !hasEnv {
		t.Errorf("re-exec environment missing %s=1: %v", urnetUpdateReexecEnv, reExecEnv)
	}

	// Verify reExecArgs carries --tag and --unit
	joinedArgs := strings.Join(reExecArgs, " ")
	if !strings.Contains(joinedArgs, "--tag "+tag) || !strings.Contains(joinedArgs, "--unit urnetwork.service") {
		t.Errorf("re-exec args missing tag or unit: %v", reExecArgs)
	}
}

func TestUpdate_ToolFirstLoopGuard(t *testing.T) {
	tempDir := t.TempDir()
	toolPath := filepath.Join(tempDir, "urnet-tools")
	if runtime.GOOS == "windows" {
		toolPath += ".exe"
	}
	_ = os.WriteFile(toolPath, fakeELF("tool-v35.5"), 0o755)

	newToolBytes := fakeELF("tool-v35.6")
	sum := sha256.Sum256(newToolBytes)
	newToolDigest := hex.EncodeToString(sum[:])

	assetName, err := runningToolAssetName()
	if err != nil {
		t.Fatal(err)
	}

	fakeRel := &releaseInfo{
		Tag:            "v3.23.0-fix.35.6",
		URL:            "http://127.0.0.1:9/provider.tar.gz",
		ProviderDigest: strings.Repeat("c", 64),
		Assets: []releaseAsset{
			{Name: assetName, Digest: "sha256:" + newToolDigest},
		},
	}

	latestReleaseMu.Lock()
	origCached := cachedLatest
	origTime := cachedLatestTime
	cachedLatest = fakeRel
	cachedLatestTime = time.Now()
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest = origCached
		cachedLatestTime = origTime
		latestReleaseMu.Unlock()
	})

	origToolVersion := ToolVersion
	ToolVersion = "v3.23.0-fix.35.5"
	t.Cleanup(func() { ToolVersion = origToolVersion })

	p := Provider{Unit: "urnetwork.service", Binary: filepath.Join(tempDir, "provider"), StateDir: tempDir}
	origDisc := verifyDiscoverFn
	verifyDiscoverFn = func() []Provider { return []Provider{p} }
	t.Cleanup(func() { verifyDiscoverFn = origDisc })

	// Set loop guard in environment
	t.Setenv(urnetUpdateReexecEnv, "1")

	origReExec := reExecToolFn
	reExecToolFn = func(exe string, args []string, env []string) error {
		t.Fatal("re-exec was called despite loop guard being set!")
		return nil
	}
	t.Cleanup(func() { reExecToolFn = origReExec })

	origSelfUpdate := selfUpdateToolFn
	selfUpdateToolFn = func(cfg updateConfig) error {
		t.Fatal("selfUpdateTool was called despite loop guard being set!")
		return nil
	}
	t.Cleanup(func() { selfUpdateToolFn = origSelfUpdate })

	// Provider update will fail since provider tarball is fake, but we only verify loop guard avoided re-exec
	_ = cmdUpdate([]string{"--unit", "urnetwork.service"}, true, false)
}

func TestUpdate_ToolFirstFailureFallsBackToRunningTool(t *testing.T) {
	tempDir := t.TempDir()
	toolPath := filepath.Join(tempDir, "urnet-tools")
	if runtime.GOOS == "windows" {
		toolPath += ".exe"
	}
	_ = os.WriteFile(toolPath, fakeELF("tool-v35.5"), 0o755)

	assetName, err := runningToolAssetName()
	if err != nil {
		t.Fatal(err)
	}

	fakeRel := &releaseInfo{
		Tag:            "v3.23.0-fix.35.6",
		URL:            "http://127.0.0.1:1/provider.tar.gz",
		ProviderDigest: strings.Repeat("c", 64),
		Assets: []releaseAsset{
			{Name: assetName, Digest: "sha256:" + strings.Repeat("0", 64)},
		},
	}

	latestReleaseMu.Lock()
	origCached := cachedLatest
	origTime := cachedLatestTime
	cachedLatest = fakeRel
	cachedLatestTime = time.Now()
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest = origCached
		cachedLatestTime = origTime
		latestReleaseMu.Unlock()
	})

	origToolVersion := ToolVersion
	ToolVersion = "v3.23.0-fix.35.5"
	t.Cleanup(func() { ToolVersion = origToolVersion })

	p := Provider{Unit: "urnetwork.service", Binary: filepath.Join(tempDir, "provider"), StateDir: tempDir}
	origDisc := verifyDiscoverFn
	verifyDiscoverFn = func() []Provider { return []Provider{p} }
	t.Cleanup(func() { verifyDiscoverFn = origDisc })

	origReExec := reExecToolFn
	reExecToolFn = func(exe string, args []string, env []string) error {
		t.Fatal("re-exec was called when self-update failed!")
		return nil
	}
	t.Cleanup(func() { reExecToolFn = origReExec })

	out := captureStdout(t, func() {
		_ = cmdUpdate([]string{"--unit", "urnetwork.service"}, true, false)
	})

	if !strings.Contains(out, "escalation unavailable") && !strings.Contains(out, "tool-first self-update failed") {
		t.Errorf("expected failure notice, got:\n%s", out)
	}
}

func TestUpdate_ToolFirstReExecFailureFallsBackToRunningTool(t *testing.T) {
	tempDir := t.TempDir()
	toolPath := filepath.Join(tempDir, "urnet-tools")
	if runtime.GOOS == "windows" {
		toolPath += ".exe"
	}
	_ = os.WriteFile(toolPath, fakeELF("tool-v35.5"), 0o755)

	newToolBytes := fakeELF("tool-v35.6")
	sum := sha256.Sum256(newToolBytes)
	newToolDigest := hex.EncodeToString(sum[:])

	assetName, err := runningToolAssetName()
	if err != nil {
		t.Fatal(err)
	}

	fakeRel := &releaseInfo{
		Tag:            "v3.23.0-fix.35.6",
		URL:            "http://127.0.0.1:1/provider.tar.gz",
		ProviderDigest: strings.Repeat("c", 64),
		Assets: []releaseAsset{
			{Name: assetName, Digest: "sha256:" + newToolDigest},
		},
	}

	latestReleaseMu.Lock()
	origCached := cachedLatest
	origTime := cachedLatestTime
	cachedLatest = fakeRel
	cachedLatestTime = time.Now()
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest = origCached
		cachedLatestTime = origTime
		latestReleaseMu.Unlock()
	})

	origToolVersion := ToolVersion
	ToolVersion = "v3.23.0-fix.35.5"
	t.Cleanup(func() { ToolVersion = origToolVersion })

	p := Provider{Unit: "urnetwork.service", Binary: filepath.Join(tempDir, "provider"), StateDir: tempDir}
	origDisc := verifyDiscoverFn
	verifyDiscoverFn = func() []Provider { return []Provider{p} }
	t.Cleanup(func() { verifyDiscoverFn = origDisc })

	origStage := stageToolForEscalationFn
	stageToolForEscalationFn = func(cfg updateConfig) string {
		sp := filepath.Join(cfg.StageDir, cfg.ToolAsset)
		_ = os.WriteFile(sp, newToolBytes, 0o755)
		return sp
	}
	t.Cleanup(func() { stageToolForEscalationFn = origStage })

	origSelfUpdate := selfUpdateToolFn
	selfUpdateToolFn = func(cfg updateConfig) error {
		return selfUpdateToolTo(toolPath, cfg)
	}
	t.Cleanup(func() { selfUpdateToolFn = origSelfUpdate })

	origReExec := reExecToolFn
	reExecToolFn = func(exe string, args []string, env []string) error {
		return os.ErrPermission
	}
	t.Cleanup(func() { reExecToolFn = origReExec })

	out := captureStdout(t, func() {
		_ = cmdUpdate([]string{"--unit", "urnetwork.service"}, true, false)
	})

	if !strings.Contains(out, "tool re-exec failed") || !strings.Contains(out, "continuing update with current tool") {
		t.Errorf("expected re-exec failure notice, got:\n%s", out)
	}
}
