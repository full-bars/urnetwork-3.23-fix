package urnettools

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderSetupElevationPinsSoleAccessible(t *testing.T) {
	oldDiscover, oldPriv, oldTTY := setupDiscoverFn, isPrivileged, stdinIsInteractiveOverride
	defer func() { setupDiscoverFn = oldDiscover; isPrivileged = oldPriv; stdinIsInteractiveOverride = oldTTY }()
	stdinIsInteractiveOverride = func() bool { return true }
	isPrivileged = func() bool { return false }
	setupDiscoverFn = func() []Provider {
		return []Provider{{Unit: "own.service", User: currentUserName(), Binary: "/own"}, {Unit: "other.service", User: "other", Binary: "/other"}}
	}
	var args []string
	opts := setupRegressionOptions()
	opts.Yes = true
	opts.Argv = []string{"extender", "setup", "--yes", "--route=capability"}
	opts.ElevateFunc = func(a []string) error { args = append([]string(nil), a...); return nil }
	opts.LookPath = func(string) (string, error) { return "sudo", nil }
	if err := RunExtenderSetup(opts); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"extender", "setup", "--yes", "--route=capability", "--unit", "own.service"}) {
		t.Fatalf("elevated args=%q", args)
	}
	isPrivileged = func() bool { return true }
	setupDiscoverFn = func() []Provider { return []Provider{{Unit: "other.service", User: "other", Binary: "/other"}} }
	_, _, _, target, err := parseExtenderSetupArgs(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	opts.Target = target
	opts.IsRoot = func() bool { return true }
	opts.ElevateFunc = func([]string) error { t.Fatal("recursive elevation"); return nil }
	if err := RunExtenderSetup(opts); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("child should refuse changed target: %v", err)
	}
}

func TestExtenderSetupElevationRefusesChangedIdentity(t *testing.T) {
	old := setupDiscoverFn
	defer func() { setupDiscoverFn = old }()
	parent := Provider{Unit: "same.service", User: "owner", Binary: "/old"}
	t.Setenv(urnetExtenderSetupTargetEnv, setupTargetIdentity([]Provider{parent}))
	setupDiscoverFn = func() []Provider { return []Provider{{Unit: "same.service", User: "other", Binary: "/new"}} }
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return true }
	opts.Target = Target{Unit: "same.service"}
	cfg := &recordingConfigurer{mockPlatformConfigurer: mockPlatformConfigurer{platform: "linux"}}
	opts.PlatformCfg = cfg
	err := RunExtenderSetup(opts)
	if err == nil || !strings.Contains(err.Error(), "target changed") || len(cfg.applied) != 0 {
		t.Fatalf("must refuse before setcap: err=%v applied=%v", err, cfg.applied)
	}
}

func TestExtenderSetupElevationBindsStateDirAndDropsOriginalTarget(t *testing.T) {
	p := Provider{StateDir: "/home/user/a b", User: "user", Binary: "/bin/provider"}
	args, err := setupBoundArgs(ExtenderSetupOptions{Argv: []string{"extender", "setup", "--user", "user", "--yes"}, Route: "capability"}, []Provider{p}, []Provider{p})
	if err != nil || !reflect.DeepEqual(args, []string{"extender", "setup", "--yes", "--route=capability", "--state-dir", "/home/user/a b"}) {
		t.Fatalf("bound args=%q err=%v", args, err)
	}
	_, _, _, target, err := parseExtenderSetupArgs(args[1:])
	if err != nil || target.StateDir != p.StateDir {
		t.Fatalf("target=%+v err=%v", target, err)
	}
}

func TestExtenderSetupElevationRefusesUnrepresentableSet(t *testing.T) {
	oldDiscover, oldPriv, oldTTY := setupDiscoverFn, isPrivileged, stdinIsInteractiveOverride
	defer func() { setupDiscoverFn = oldDiscover; isPrivileged = oldPriv; stdinIsInteractiveOverride = oldTTY }()
	isPrivileged = func() bool { return true }
	stdinIsInteractiveOverride = func() bool { return true }
	setupDiscoverFn = func() []Provider { return []Provider{{User: "one", Binary: "/one"}, {User: "two", Binary: "/two"}} }
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return false }
	opts.Yes = true
	opts.Argv = []string{"extender", "setup", "--yes", "--route=capability"}
	opts.ElevateFunc = func([]string) error { t.Fatal("must not elevate unrepresentable set"); return nil }
	opts.LookPath = func(string) (string, error) { return "sudo", nil }
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	err := RunExtenderSetup(opts)
	if err == nil || !strings.Contains(err.Error(), "cannot safely elevate") || !strings.Contains(err.Error(), "--unit") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
}
