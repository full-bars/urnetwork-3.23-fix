package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatusPanelRenders runs the real renderStatusPanel path and asserts
// the key sections appear, so regressions in formatting break CI.
func TestStatusPanelRenders(t *testing.T) {
	tmp := t.TempDir()
	writeTestProxyState(t, tmp)

	p := Provider{
		User:       "user",
		Unit:       "urnetwork.service",
		Binary:     `C:\Users\user\AppData\Local\urnetwork\provider\urnetwork.exe`,
		Version:    "v3.23.0-fix.30.3",
		StateDir:   tmp,
		PID:        4212,
		Running:    true,
		Network:    "example-net",
		NetworkID:  "abcd-1234",
		JWTExpires: time.Now().Add(24 * time.Hour),
	}
	out := capturePanel(t, p)
	for _, want := range []string{
		"PROVIDER STATUS", "example-net", "RUNNING",
		"user:", "binary:", "state dir:", "urnetwork",
		"PROXIES:", "URL sources:", "file sources:", "proxies.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q:\n%s", want, out)
		}
	}
	// Proxy count: fixture has 5 non-blank lines, 3 up.
	if !strings.Contains(out, "3 up / 5 total") {
		t.Errorf("expected '3 up / 5 total', got:\n%s", out)
	}
}

// TestStatusPanelStopped checks a stopped provider (pid 0, STOPPED badge).
func TestStatusPanelStopped(t *testing.T) {
	tmp := t.TempDir()
	p := Provider{
		User:     "user",
		StateDir: tmp,
		PID:      0,
	}
	out := capturePanel(t, p)
	if !strings.Contains(out, "STOPPED") {
		t.Errorf("expected STOPPED, got:\n%s", out)
	}
	if strings.Contains(out, "RUNNING") {
		t.Errorf("stopped panel should not say RUNNING:\n%s", out)
	}
	// pid 0 renders as "-" not literal 0
	if strings.Contains(out, "pid:         0\n") {
		t.Errorf("pid 0 should render as '-':\n%s", out)
	}
	// no state dir -> proxies n/a
	if !strings.Contains(out, "n/a  (no proxy health state)") {
		t.Errorf("expected n/a proxies, got:\n%s", out)
	}
}

// capturePanel runs renderStatusPanel capturing stdout.
func capturePanel(t *testing.T, p Provider) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	renderStatusPanel(p)
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

func writeTestProxyState(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	health := "addr1:up\naddr2:up\naddr3:down\naddr4:up\naddr5:down\n"
	os.WriteFile(filepath.Join(dir, "proxy_health.state"), []byte(health), 0o644)
	urls := `{"sources":["https://dl.fullbars.xyz/proxies.txt"]}`
	os.WriteFile(filepath.Join(dir, "proxy_url.json"), []byte(urls), 0o644)
	ps := `{"source":"C:\\Users\\user\\proxies.txt"}`
	os.WriteFile(filepath.Join(dir, "proxy.state"), []byte(ps), 0o644)
}
