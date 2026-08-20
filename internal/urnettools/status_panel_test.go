package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Generic counterpart to render_demo_test.go's
// capturePanel, used here for the lower-level printProxyStatus.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

// TestClamp covers the truncation helper used to bound status-panel values.
func TestClamp(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"shorter than max", "abc", 10, "abc"},
		{"exact max", "abcde", 5, "abcde"},
		{"one over max", "abcdef", 5, "ab..."},
		{"much longer", strings.Repeat("x", 100), 10, "xxxxxxx..."},
		{"empty string", "", 5, ""},
		{"max exactly three", "abcdef", 3, "..."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clamp(c.s, c.max)
			if got != c.want {
				t.Errorf("clamp(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
			}
			if len(got) > c.max {
				t.Errorf("clamp(%q, %d) = %q, length %d exceeds max %d", c.s, c.max, got, len(got), c.max)
			}
		})
	}
}

// TestStatusLineUp covers the token-matching used to classify a proxy
// health line as "up" or "down".
func TestStatusLineUp(t *testing.T) {
	positive := []string{
		"addr1:up",
		"10.0.0.1 up",
		"10.0.0.1 up: latency 5ms",
		"proxy1 up ok",
		"proxy2 healthy",
		"some-proxy ok",
	}
	for _, ln := range positive {
		if !statusLineUp(ln) {
			t.Errorf("statusLineUp(%q) = false, want true", ln)
		}
	}

	negative := []string{
		"addr1:down",
		"proxy1 dead",
		"proxy2 failed",
		"proxy3 timeout",
		"",
	}
	for _, ln := range negative {
		if statusLineUp(ln) {
			t.Errorf("statusLineUp(%q) = true, want false", ln)
		}
	}
}

// TestStatusLineUpSubstringQuirk documents the fixed behavior: status tokens
// ("ok"/"up"/"healthy") are matched on word boundaries only, so words that
// merely contain "ok" (e.g. "broken") are NOT misclassified as up.
func TestStatusLineUpSubstringQuirk(t *testing.T) {
	if statusLineUp("proxy1 broken") {
		t.Errorf("statusLineUp(\"proxy1 broken\") = true, want false")
	}
}

// TestReadProxyHealth covers the proxy_health.state parser.
func TestReadProxyHealth(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		up, total, ok := readProxyHealth(t.TempDir())
		if ok {
			t.Errorf("ok = true for missing file")
		}
		if up != 0 || total != 0 {
			t.Errorf("up=%d total=%d, want 0,0", up, total)
		}
	})

	t.Run("blank content only", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "\n\n   \n")
		_, _, ok := readProxyHealth(dir)
		if ok {
			t.Errorf("ok = true for all-blank content, want false (total stays 0)")
		}
	})

	t.Run("mixed up and down", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "a:up\nb:down\nc:up\nd:down\ne:up\n")
		up, total, ok := readProxyHealth(dir)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if up != 3 || total != 5 {
			t.Errorf("up=%d total=%d, want 3,5", up, total)
		}
	})

	t.Run("all down", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "a:down\nb:down\n")
		up, total, ok := readProxyHealth(dir)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if up != 0 || total != 2 {
			t.Errorf("up=%d total=%d, want 0,2", up, total)
		}
	})

	t.Run("no trailing newline", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "a:up")
		up, total, ok := readProxyHealth(dir)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if up != 1 || total != 1 {
			t.Errorf("up=%d total=%d, want 1,1", up, total)
		}
	})
}

// TestReadProxyURLSources covers the proxy_url.json reader.
func TestReadProxyURLSources(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if got := readProxyURLSources(t.TempDir()); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_url.json", "{not valid json")
		if got := readProxyURLSources(dir); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("empty sources array", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_url.json", `{"sources":[]}`)
		if got := readProxyURLSources(dir); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("missing sources field", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_url.json", `{"other":"field"}`)
		if got := readProxyURLSources(dir); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("multiple sources", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_url.json", `{"sources":["https://a.example/p.txt","https://b.example/p.txt"]}`)
		got := readProxyURLSources(dir)
		want := []string{"https://a.example/p.txt", "https://b.example/p.txt"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("source[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

// TestReadProxyFileSource covers the proxy.state file-source reader.
func TestReadProxyFileSource(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		p := Provider{StateDir: t.TempDir()}
		if got := readProxyFileSource(p); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy.state", "not json")
		p := Provider{StateDir: dir}
		if got := readProxyFileSource(p); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty source field", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy.state", `{"source":""}`)
		p := Provider{StateDir: dir}
		if got := readProxyFileSource(p); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("valid source with windows path", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy.state", `{"source":"C:\\Users\\full-bars\\proxies.txt"}`)
		p := Provider{StateDir: dir}
		want := `C:\Users\full-bars\proxies.txt`
		if got := readProxyFileSource(p); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestPrintProxyStatus exercises the proxy summary section directly,
// independent of the surrounding panel, across all degrade-gracefully
// combinations.
func TestPrintProxyStatus(t *testing.T) {
	t.Run("no state at all", func(t *testing.T) {
		p := Provider{StateDir: t.TempDir()}
		out := captureStdout(t, func() { printProxyStatus(p) })
		if !strings.Contains(out, "n/a  (no proxy health state)") {
			t.Errorf("missing proxies n/a line:\n%s", out)
		}
		if !strings.Contains(out, "URL sources:") || !strings.Contains(out, "none") {
			t.Errorf("expected 'none' for URL sources:\n%s", out)
		}
		if !strings.Contains(out, "file sources:") {
			t.Errorf("missing file sources label:\n%s", out)
		}
	})

	t.Run("health present, no sources", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "a:up\nb:down\n")
		p := Provider{StateDir: dir}
		out := captureStdout(t, func() { printProxyStatus(p) })
		if !strings.Contains(out, "1 up / 2 total") {
			t.Errorf("expected '1 up / 2 total':\n%s", out)
		}
		if !strings.Contains(out, "[UP]") {
			t.Errorf("expected [UP] badge when at least one proxy is up:\n%s", out)
		}
	})

	t.Run("health present but all down reports DOWN badge", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_health.state", "a:down\nb:down\n")
		p := Provider{StateDir: dir}
		out := captureStdout(t, func() { printProxyStatus(p) })
		if !strings.Contains(out, "0 up / 2 total") {
			t.Errorf("expected '0 up / 2 total':\n%s", out)
		}
		if !strings.Contains(out, "[DOWN]") {
			t.Errorf("expected [DOWN] badge when no proxies are up:\n%s", out)
		}
	})

	t.Run("url sources only", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy_url.json", `{"sources":["https://example.com/list.txt"]}`)
		p := Provider{StateDir: dir}
		out := captureStdout(t, func() { printProxyStatus(p) })
		if !strings.Contains(out, "https://example.com/list.txt") {
			t.Errorf("missing URL source in output:\n%s", out)
		}
		// File source should still degrade to "none".
		lines := strings.Split(out, "\n")
		found := false
		for _, ln := range lines {
			if strings.Contains(ln, "file sources:") && strings.Contains(ln, "none") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'file sources: ... none' line:\n%s", out)
		}
	})

	t.Run("file source only", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "proxy.state", `{"source":"/etc/urnetwork/proxies.txt"}`)
		p := Provider{StateDir: dir}
		out := captureStdout(t, func() { printProxyStatus(p) })
		if !strings.Contains(out, "/etc/urnetwork/proxies.txt") {
			t.Errorf("missing file source in output:\n%s", out)
		}
		lines := strings.Split(out, "\n")
		found := false
		for _, ln := range lines {
			if strings.Contains(ln, "URL sources:") && strings.Contains(ln, "none") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'URL sources: ... none' line:\n%s", out)
		}
	})
}

// TestRenderStatusPanelTitleFallsBackToUser checks the header title uses
// the network name when present, and falls back to the user when the
// provider has no resolved network (e.g. auth not yet completed).
func TestRenderStatusPanelTitleFallsBackToUser(t *testing.T) {
	p := Provider{
		User:     "full-bars",
		StateDir: t.TempDir(),
	}
	out := capturePanel(t, p)
	if !strings.Contains(out, "PROVIDER STATUS   full-bars") {
		t.Errorf("expected title to fall back to user 'full-bars':\n%s", out)
	}
}

// TestRenderStatusPanelRunningColorAndBadge checks the ANSI color wrapper
// and "@"/"O" badges are applied based on Provider.Running.
func TestRenderStatusPanelRunningColorAndBadge(t *testing.T) {
	running := Provider{User: "u", StateDir: t.TempDir(), Running: true}
	out := capturePanel(t, running)
	if !strings.Contains(out, "\x1b[32mRUNNING\x1b[0m") {
		t.Errorf("expected green-colored RUNNING, got:\n%s", out)
	}
	if !strings.Contains(out, "@ ") {
		t.Errorf("expected '@' badge for running provider:\n%s", out)
	}

	stopped := Provider{User: "u", StateDir: t.TempDir(), Running: false}
	out2 := capturePanel(t, stopped)
	if strings.Contains(out2, "\x1b[32m") {
		t.Errorf("stopped provider should not carry the running color code:\n%s", out2)
	}
	if !strings.Contains(out2, "O ") {
		t.Errorf("expected 'O' badge for stopped provider:\n%s", out2)
	}
}

// TestRenderStatusPanelTruncatesLongValues verifies that overlong field
// values (e.g. an unusually long binary path) are clamped in the rendered
// row rather than overflowing the panel unbounded.
func TestRenderStatusPanelTruncatesLongValues(t *testing.T) {
	longBinary := "C:\\" + strings.Repeat("very-long-segment\\", 10) + "urnetwork.exe"
	p := Provider{
		User:     "u",
		Binary:   longBinary,
		StateDir: t.TempDir(),
	}
	out := capturePanel(t, p)
	if strings.Contains(out, longBinary) {
		t.Errorf("expected long binary path to be truncated, but full path appeared verbatim:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("expected truncation marker '...' in output:\n%s", out)
	}
}

// TestRenderStatusPanelJWTExpiresFormatting checks both the "n/a" zero-value
// case and the RFC3339-formatted case.
func TestRenderStatusPanelJWTExpiresFormatting(t *testing.T) {
	t.Run("zero value renders n/a", func(t *testing.T) {
		p := Provider{User: "u", StateDir: t.TempDir()}
		out := capturePanel(t, p)
		if !strings.Contains(out, "jwt expires: n/a") {
			t.Errorf("expected 'jwt expires: n/a', got:\n%s", out)
		}
	})

	t.Run("set value renders RFC3339", func(t *testing.T) {
		exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
		p := Provider{User: "u", StateDir: t.TempDir(), JWTExpires: exp}
		out := capturePanel(t, p)
		if !strings.Contains(out, exp.Format(time.RFC3339)) {
			t.Errorf("expected formatted jwt expiry %q, got:\n%s", exp.Format(time.RFC3339), out)
		}
	})
}

// writeFile is a small test helper for writing a fixture file into dir,
// failing the test on error.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
