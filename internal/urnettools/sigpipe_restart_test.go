package urnettools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sigpipeOutput(restart, force, prevent string) string {
	return fmt.Sprintf("Restart=%s\nRestartForceExitStatus=%s\nRestartPreventExitStatus=%s\nSuccessExitStatus=\n", restart, force, prevent)
}

func TestSigpipeState(t *testing.T) {
	for _, tc := range []struct {
		name, restart, force, prevent string
		risk                          bool
	}{
		{"always", "always", "", "", false},
		{"on-success", "on-success", "", "", false},
		{"on-failure", "on-failure", "", "", true},
		{"no", "no", "", "", true},
		{"empty", "", "", "", true},
		{"force tokens", "no", "SIGPIPE 13", "", false},
		{"prevent wins", "always", "SIGPIPE 13", "13 SIGPIPE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := parseSigpipeState(sigpipeOutput(tc.restart, tc.force, tc.prevent))
			if err != nil || state.risk() != tc.risk {
				t.Fatalf("state=%+v err=%v", state, err)
			}
		})
	}
	if _, err := parseSigpipeState("Restart=no\n"); err == nil {
		t.Fatal("missing properties accepted")
	}
}

func TestSigpipeReconcile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "unit.d")
	p := Provider{Unit: "example.service", StateHome: root}
	dirFor := func(Provider) (string, error) { return dir, nil }
	output := sigpipeOutput("on-failure", "", "")
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "show" {
			t.Fatalf("wrong systemctl argv: %q", args)
		}
		return []byte(output), nil
	}
	reloads := 0
	reload := func(Provider) error { reloads++; output = sigpipeOutput("on-failure", "SIGPIPE", ""); return nil }
	changed, err := reconcileSigpipeRestart(p, run, dirFor, reload)
	if err != nil || !changed || reloads != 1 {
		t.Fatalf("first write: changed=%v reloads=%d err=%v", changed, reloads, err)
	}
	path := filepath.Join(dir, sigpipeDropInName)
	if b, err := os.ReadFile(path); err != nil || string(b) != sigpipeDropInContent {
		t.Fatalf("content=%q err=%v", b, err)
	}
	changed, err = reconcileSigpipeRestart(p, run, dirFor, reload)
	if err != nil || changed || reloads != 1 {
		t.Fatalf("second: changed=%v reloads=%d err=%v", changed, reloads, err)
	}
	if err := os.WriteFile(path, []byte("wrong\n"), 0644); err != nil {
		t.Fatal(err)
	}
	output = sigpipeOutput("no", "", "")
	changed, err = reconcileSigpipeRestart(p, run, dirFor, reload)
	if err != nil || !changed || reloads != 2 {
		t.Fatalf("rewrite: changed=%v reloads=%d err=%v", changed, reloads, err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != sigpipeDropInContent {
		t.Fatalf("rewrite content=%q err=%v", b, err)
	}
	output = sigpipeOutput("no", "SIGPIPE", "SIGPIPE")
	changed, err = reconcileSigpipeRestart(p, run, dirFor, reload)
	if changed || err == nil || !strings.Contains(err.Error(), "RestartPreventExitStatus") || reloads != 2 {
		t.Fatalf("prevent: changed=%v reloads=%d err=%v", changed, reloads, err)
	}
	if err := os.WriteFile(path, []byte("wrong\n"), 0644); err != nil {
		t.Fatal(err)
	}
	output = sigpipeOutput("no", "", "")
	changed, err = reconcileSigpipeRestart(p, run, dirFor, func(Provider) error { return nil })
	if !changed || err == nil || !strings.Contains(err.Error(), "ineffective") || !strings.Contains(err.Error(), "Restart=no") {
		t.Fatalf("override: changed=%v err=%v", changed, err)
	}
}
