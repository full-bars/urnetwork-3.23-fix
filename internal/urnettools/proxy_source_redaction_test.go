package urnettools

import (
	"strings"
	"testing"
)

func TestDashboardProxySourceRedaction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "proxy_url.json", `{"sources":["alice:secret@proxy:bad","https://user:pass@api.example.com/lists/TOKEN?key=TOKEN","user:pass@proxy:1080?key=a@TOKEN"]}`)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldP, oldS := discoverProcessesFn, discoverStoppedFn
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn = oldP, oldS })
	discoverProcessesFn = func() []Provider { return []Provider{{User: "test", StateDir: dir}} }
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	var err error
	out := captureStdout(t, func() { err = cmdDashboard(nil) })
	if err != nil {
		t.Fatal(err)
	}
	checkRedactedSources(t, out)
}

func TestPlainProxyStatusSourceRedaction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "proxy_url.json", `{"sources":["alice:secret@proxy:bad","https://user:pass@api.example.com/lists/TOKEN?key=TOKEN","user:pass@proxy:1080?key=a@TOKEN"]}`)
	out := captureStdout(t, func() { printProxyStatus(Provider{StateDir: dir}) })
	checkRedactedSources(t, out)
}

func TestSanitizeSourceForDisplay(t *testing.T) {
	cases := []struct{ input, want string }{
		{"https://user:pass@api.example.com/lists/TOKEN?key=TOKEN#TOKEN", "https://api.example.com"},
		{"https://api.example.com/lists/TOKEN", "https://api.example.com"},
		{"user:pass@proxy:1080?key=a@TOKEN", "proxy:1080"},
		{"alice:secret@proxy:bad", "proxy:bad"},
		{"proxy:1080?key=TOKEN", "proxy:1080"},
		{"user:pass@", "<unparsed source>"},
		{"user:pass@?key=a@TOKEN", "<unparsed source>"},
		{"user:pass", "<unparsed source>"},
		{"bad path/TOKEN", "<unparsed source>"},
		{"[2001:db8::1]:1080", "[2001:db8::1]:1080"},
		{"https://user:pass@[2001:db8::1]:1080/lists/TOKEN", "https://[2001:db8::1]:1080"},
	}
	for _, tc := range cases {
		if got := sanitizeSourceForDisplay(tc.input); got != tc.want {
			t.Errorf("sanitizeSourceForDisplay(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func checkRedactedSources(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{"proxy:bad", "https://api.example.com", "proxy:1080"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, secret := range []string{"alice", "secret", "user:pass", "TOKEN", "/lists/", "?key="} {
		if strings.Contains(out, secret) {
			t.Errorf("leaked %q in %q", secret, out)
		}
	}
}
