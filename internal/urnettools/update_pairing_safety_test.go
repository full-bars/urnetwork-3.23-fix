//go:build unix

package urnettools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateUnitReconcileFailureNeverRestarts(t *testing.T) {
	for _, tc := range []struct{ name, unit, version string }{
		{"migration", simpleUnit, "v3.23.0-fix.31.1"},
		{"demotion", "[Service]\nType=notify\n", "v3.23.0-fix.30.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCapTools(t)
			stubVerifyLoop(t)
			p, _, _ := fakeUnit(t, tc.unit)
			daemonReloadFunc = func(Provider) error { return errors.New("reload failed") }
			verifyProviderVersionFn = func(string) string { return tc.version }
			oldRestart := restartForUpdate
			restarted := false
			restartForUpdate = func(Provider) error { restarted = true; return nil }
			t.Cleanup(func() { restartForUpdate = oldRestart })
			dir := t.TempDir()
			p.Binary = filepath.Join(dir, "provider")
			p.StateDir = dir
			p.User = ""
			if err := os.WriteFile(p.Binary, []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			elf := append([]byte("\x7fELF"), make([]byte, 64)...)
			var tarData, gzData bytes.Buffer
			tw := tar.NewWriter(&tarData)
			if err := tw.WriteHeader(&tar.Header{Name: tarRelPath(runtime.GOOS, runtimeGOARCH()), Mode: 0755, Size: int64(len(elf))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(elf); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(&gzData)
			if _, err := gz.Write(tarData.Bytes()); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(gzData.Bytes()) }))
			defer srv.Close()
			cfg := updateConfig{Tag: "v9.9.9-test", Digest: fmt.Sprintf("%x", sha256.Sum256(gzData.Bytes())), AssetURL: srv.URL, StageDir: filepath.Join(dir, "stage")}
			err := updateProvider(p, cfg)
			if err == nil || !strings.Contains(err.Error(), "no restart or HotSwap was attempted") || !strings.Contains(err.Error(), "daemon-reload") {
				t.Fatalf("wrong error: %v", err)
			}
			if restarted {
				t.Fatal("restarted after reconciliation failure")
			}
			got, err := os.ReadFile(p.Binary)
			if err != nil || !bytes.Equal(got, elf) {
				t.Fatalf("swapped binary: %q %v", got, err)
			}
		})
	}
}

func TestRollbackDemotionFailureLeavesProviderAlone(t *testing.T) {
	fakeCapTools(t)
	stubVerifyLoop(t)
	p, _, _ := fakeUnit(t, "[Service]\nType=notify\n")
	daemonReloadFunc = func(Provider) error { return errors.New("reload failed") }
	verifyDiscoverFn = func() []Provider { return nil }
	verifyPidIsAliveFn = func(int) bool { return true }
	verifyProviderVersionFn = func(string) string { return "v3.23.0-fix.30.9" }
	dir := t.TempDir()
	p.Binary = filepath.Join(dir, "provider")
	p.StateDir = dir
	p.PID = 100
	p.User = ""
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(p.Binary, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	oldRestart := restartForUpdate
	restarted := false
	restartForUpdate = func(Provider) error { restarted = true; return nil }
	t.Cleanup(func() { restartForUpdate = oldRestart })
	err := verifyRestartLoop(p, testConfig("v9.9.9-test"), true, backup)
	if err == nil || !strings.Contains(err.Error(), "unit demotion failed") || !strings.Contains(err.Error(), "Do not restart") {
		t.Fatalf("wrong error: %v", err)
	}
	if restarted {
		t.Fatal("restarted after rollback demotion failure")
	}
	got, err := os.ReadFile(p.Binary)
	if err != nil || string(got) != "old" {
		t.Fatalf("rollback: %q %v", got, err)
	}
}

func TestHotSwapRollbackFailureReportsObservationAndDoesNotDemote(t *testing.T) {
	stubVerifyLoop(t)
	p := testProvider(t.TempDir(), 100)
	p.Binary = filepath.Join(p.StateDir, "provider")
	if err := os.WriteFile(p.Binary, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(p.StateDir, "missing-backup")
	verifyDiscoverFn = func() []Provider { return nil }
	verifyPidIsAliveFn = func(int) bool { return true }
	verifyProviderVersionFn = func(string) string {
		t.Fatal("must not inspect or demote after a failed rollback")
		return ""
	}
	err := verifyRestartLoop(p, testConfig("new"), true, backup)
	if err == nil {
		t.Fatal("expected rollback failure")
	}
	for _, want := range []string{"no matching provider entry was discovered", "rollback from " + backup + " failed", "installed binary remains at " + p.Binary, "provider PID 100 was not restarted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	got, readErr := os.ReadFile(p.Binary)
	if readErr != nil || string(got) != "new" {
		t.Fatalf("installed binary: %q %v", got, readErr)
	}
}

func TestReconcileRejectsEmptyUnitType(t *testing.T) {
	p, _, _ := fakeUnit(t, simpleUnit)
	unitTypeFunc = func(Provider) (string, error) { return "", nil }
	for _, version := range []string{"v3.23.0-fix.30.9", "v3.23.0-fix.31.1"} {
		if _, err := reconcileUnitTypeForBinary(p, version); err == nil || !strings.Contains(err.Error(), "empty type") {
			t.Fatalf("version %s: got %v", version, err)
		}
	}
}

func TestDemotionRejectsNotifyDropIn(t *testing.T) {
	p, _, _ := fakeUnit(t, "[Service]\nType=notify\n")
	unitTypeFunc = func(Provider) (string, error) { return "notify", nil }
	if _, err := demoteUnitToSimple(p); err == nil || !strings.Contains(err.Error(), "remains Type=notify") {
		t.Fatalf("got %v", err)
	}
}
