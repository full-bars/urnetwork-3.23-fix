package urnettools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCapTools puts recording getcap/setcap/sudo stand-ins first on PATH and
// returns the log they append to. The first getcap reports a capability on
// the old binary, so a provider swap must try to restore it.
func fakeCapTools(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("extender capability is only restored on linux")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> '" + log + "'\n"
	for _, name := range []string{"getcap", "setcap", "sudo"} {
		body := script
		if name == "getcap" {
			body += "if [ ! -f '" + log + ".checked' ]; then touch '" + log + ".checked'; printf '%s cap_net_bind_service=ep\\n' \"$1\"; fi\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func requireSetcapFor(t *testing.T, log, dst string) {
	t.Helper()
	data, _ := os.ReadFile(log)
	for _, line := range strings.Split(string(data), "\n") {
		if (strings.HasPrefix(line, "setcap ") || strings.HasPrefix(line, "sudo -n setcap ")) &&
			strings.Contains(line, "cap_net_bind_service=+ep") && strings.HasSuffix(line, " "+dst) {
			return
		}
	}
	t.Fatalf("no setcap for %s after the binary swap; cap tool calls:\n%s", dst, data)
}

// TestUpdateProviderSwapRestoresExtenderCapability pins the update call site
// (updateProvider's swap): reverting it to installBinary skips the restore.
func TestUpdateProviderSwapRestoresExtenderCapability(t *testing.T) {
	log := fakeCapTools(t)
	stubVerifyLoop(t)
	verifyDiscoverFn = func() []Provider { return nil }
	origRestart := restartForUpdate
	restartForUpdate = func(Provider) error { return nil }
	t.Cleanup(func() { restartForUpdate = origRestart })

	dir := t.TempDir()
	dst := filepath.Join(dir, "provider")
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	elf := append([]byte("\x7fELF"), make([]byte, 64)...)
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: tarRelPath(runtime.GOOS, runtimeGOARCH()), Mode: 0o755, Size: int64(len(elf))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(elf); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(gzBuf.Bytes()) }))
	defer srv.Close()

	cfg := updateConfig{
		Tag:      "v9.9.9-test",
		Digest:   fmt.Sprintf("%x", sha256.Sum256(gzBuf.Bytes())),
		AssetURL: srv.URL + "/a.tar.gz",
		StageDir: filepath.Join(dir, "stage"),
	}
	// The verify loop's outcome is irrelevant here; only the swap matters.
	_ = updateProvider(Provider{Binary: dst, StateDir: dir}, cfg)

	if got, _ := os.ReadFile(dst); !bytes.Equal(got, elf) {
		t.Fatalf("binary was not swapped: %q", got)
	}
	requireSetcapFor(t, log, dst)
}

// TestHotSwapRollbackRestoresExtenderCapability pins the rollback call site
// (verifyRestartLoop's backup restore): reverting it to installBinary skips
// the restore.
func TestHotSwapRollbackRestoresExtenderCapability(t *testing.T) {
	log := fakeCapTools(t)
	stubVerifyLoop(t)
	verifyDiscoverFn = func() []Provider { return nil }
	verifyPidIsAliveFn = func(int) bool { return true } // old PID alive: rollback branch
	verifyProviderVersionFn = func(string) string { return "" }

	dir := t.TempDir()
	dst, backup := filepath.Join(dir, "provider"), filepath.Join(dir, "provider.bak")
	if err := os.WriteFile(dst, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Provider{StateDir: dir, Binary: dst, PID: 100}
	if err := verifyRestartLoop(p, testConfig("v9.9.9-test"), true, backup); err == nil {
		t.Fatal("expected the failed HotSwap to return an error")
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Fatalf("binary was not rolled back: %q", got)
	}
	requireSetcapFor(t, log, dst)
}
