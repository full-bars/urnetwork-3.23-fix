package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNormalizeProxyLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Bare formats
		{"bare host:port", "1.2.3.4:1080", "1.2.3.4:1080::"},
		{"bare host:port with spaces", "  1.2.3.4:1080  ", "1.2.3.4:1080::"},

		// socks5:// scheme
		{"socks5://host:port", "socks5://1.2.3.4:1080", "1.2.3.4:1080::"},
		{"socks5://user:pass@host:port", "socks5://myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080:myuser:mypass"},
		{"socks5:// upper", "SOCKS5://1.2.3.4:1080", "1.2.3.4:1080::"},

		// user:pass@host:port
		{"user:pass@host:port", "myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080:myuser:mypass"},

		// host:port:user:pass (already canonical)
		{"canonical host:port:user:pass", "1.2.3.4:1080:myuser:mypass", "1.2.3.4:1080:myuser:mypass"},
		{"canonical empty auth", "1.2.3.4:1080::", "1.2.3.4:1080::"},

		// host:port user:pass (space-separated)
		{"space separated user:pass", "1.2.3.4:1080 myuser:mypass", "1.2.3.4:1080:myuser:mypass"},

		// socks5:// with path (should strip)
		{"socks5 with path", "socks5://1.2.3.4:1080/path", "1.2.3.4:1080::"},

		// Rejected formats
		{"socks4 rejected", "socks4://1.2.3.4:1080", ""},
		{"http rejected", "http://1.2.3.4:1080", ""},
		{"https rejected", "https://1.2.3.4:1080", ""},
		{"empty line", "", ""},
		{"comment line", "# this is a comment", ""},
		{"bare hostname no port", "1.2.3.4", ""},
		{"invalid port", "1.2.3.4:abc", ""},
		// Port range validation: 1-65535 valid; 0 and >65535 rejected.
		{"port 0 rejected", "1.2.3.4:0", ""},
		{"port min valid", "1.2.3.4:1", "1.2.3.4:1::"},
		{"port 65535 valid", "1.2.3.4:65535", "1.2.3.4:65535::"},
		{"port 65536 rejected", "1.2.3.4:65536", ""},
		{"port 99999 rejected", "1.2.3.4:99999", ""},
		{"ipv6 port 0 rejected", "[::1]:0", ""},
		{"ipv6 port 65535 valid", "[::1]:65535", "[::1]:65535::"},

		// IPv6 — hosts with colons are returned BRACKETED (net.SplitHostPort
		// rejects unbracketed IPv6: "too many colons in address")
		{"ipv6 brackets", "[::1]:1080", "[::1]:1080::"},
		// Bare unbracketed IPv6 host is ambiguous (last hextet vs port) — rejected
		{"ipv6 bare rejected", "::1:1080", ""},

		// Colon-in-credential rejection — host:port:user:pass cannot represent
		// colons inside user/pass (parseProxyAddress splits on last two colons
		// and would misparse/drop them -> proxy never authenticates).
		{"colon in password user:pass@", "user:p:ass@1.2.3.4:1080", ""},
		{"colon in user user:pass@", "u:ser:ass@1.2.3.4:1080", ""},
		{"colon in password space sep", "1.2.3.4:1080 user:p:ass", ""},
		{"normal creds pass", "user:pass@1.2.3.4:1080", "1.2.3.4:1080:user:pass"},

		// URL source (should be filtered by isSourceURL, not normalizeProxyLine)
		{"url source", "https://raw.githubusercontent.com/foo/bar.txt", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeProxyLine(tt.input)
			if got != tt.want {
				t.Errorf("normalizeProxyLine(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseProxyList(t *testing.T) {
	// TXT format
	txtContent := `# comment line
1.2.3.4:1080
5.6.7.8:1080:user:pass
socks5://9.10.11.12:1080
`
	result := parseProxyList(txtContent)
	if len(result) != 3 {
		t.Errorf("parseProxyList txt: got %d results, want 3: %v", len(result), result)
	}

	// CSV format (ip,port,user,pass)
	csvContent := `ip,port,user,pass
1.2.3.4,1080,user1,pass1
5.6.7.8,1080,,
`
	result = parseProxyList(csvContent)
	if len(result) != 2 {
		t.Errorf("parseProxyList csv: got %d results, want 2: %v", len(result), result)
	}
	if result[0] != "1.2.3.4:1080:user1:pass1" {
		t.Errorf("parseProxyList csv[0] = %q, want 1.2.3.4:1080:user1:pass1", result[0])
	}
	if result[1] != "5.6.7.8:1080::" {
		t.Errorf("parseProxyList csv[1] = %q, want 5.6.7.8:1080::", result[1])
	}
}

func TestIsSourceURL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"https://raw.githubusercontent.com/foo/bar.txt", true},
		{"http://example.com/proxies.csv", true},
		{"1.2.3.4:1080", false},
		{"socks5://1.2.3.4:1080", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isSourceURL(tt.input)
			if got != tt.want {
				t.Errorf("isSourceURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeDeduplication(t *testing.T) {
	// Same proxy in different formats should normalize to the same string
	inputs := []string{
		"socks5://1.2.3.4:1080",
		"1.2.3.4:1080",
		"1.2.3.4:1080::",
		"  1.2.3.4:1080  ",
	}
	var normalized []string
	for _, input := range inputs {
		n := normalizeProxyLine(input)
		if n == "" {
			t.Errorf("normalizeProxyLine(%q) = empty, want non-empty", input)
			continue
		}
		normalized = append(normalized, n)
	}
	// All should normalize to the same form
	for i := 1; i < len(normalized); i++ {
		if normalized[i] != normalized[0] {
			t.Errorf("normalizeProxyLine(%q) = %q, want %q (same as first input)",
				inputs[i], normalized[i], normalized[0])
		}
	}
}

// TestDedupDirectWins locks in the dedup ordering: a proxy pasted directly
// AND appearing inside a fetched URL source keeps the direct entry; the
// fetched copy is deduped out. Direct lines are processed before URL sources.
func TestDedupDirectWins(t *testing.T) {
	input := []string{
		"socks5://1.2.3.4:1080",        // direct line -> normalized 1.2.3.4:1080::
		"1.2.3.4:1080",                 // duplicate direct line -> skipped
		"https://example.com/list.txt", // URL source returning the same proxy
	}
	// Simulate the paste flow's dedup ordering without the exec/fetch side
	// effects: URLs are separated from direct lines (and not fetched in this
	// test — the fetched list is faked below). Direct lines process first.
	var directLines []string
	for _, l := range input {
		if !isSourceURL(l) {
			directLines = append(directLines, l)
		}
	}
	seen := map[string]bool{}
	var normalized []string
	for _, l := range directLines {
		if n := normalizeProxyLine(l); n != "" && !seen[n] {
			seen[n] = true
			normalized = append(normalized, n)
		}
	}
	// The URL source (here: a fetched list containing the same proxy) must be
	// deduped out — already seen from the direct line.
	fetchedContent := "1.2.3.4:1080\n"
	fetched := parseProxyList(fetchedContent)
	added := 0
	for _, p := range fetched {
		if !seen[p] {
			seen[p] = true
			normalized = append(normalized, p)
			added++
		}
	}
	if added != 0 {
		t.Errorf("expected URL-sourced duplicate to be deduped, got %d added", added)
	}
	if len(normalized) != 1 || normalized[0] != "1.2.3.4:1080::" {
		t.Errorf("expected exactly 1.2.3.4:1080:: after dedup, got %v", normalized)
	}
}

// TestParseProxyListHeuristicUnion: a TXT list whose credentials contain commas
// must NOT lose entries to the >50%-comma CSV heuristic. Regression for the
// silent data-drop bug (3 valid lines → only 1 recovered).
func TestParseProxyListHeuristicUnion(t *testing.T) {
	in := "1.2.3.4:1080:user1,name:pass1\n5.6.7.8:1080:user2,name:pass2\n9.10.11.12:1080::\n"
	got := parseProxyList(in)
	if len(got) != 3 {
		t.Errorf("parseProxyList dropped entries: got %d, want 3: %v", len(got), got)
	}
}

// TestRedactProxyLine: rejected-line diagnostics must not leak credentials.
func TestRedactProxyLine(t *testing.T) {
	for _, in := range []string{
		"user:p:ass@1.2.3.4:1080",
		"1.2.3.4:1080 user:p:ass",
	} {
		out := redactProxyLine(in)
		if out == in {
			t.Errorf("redactProxyLine(%q) = %q: unchanged, credential leaked", in, out)
		}
	}
	if got := redactProxyLine("user:p:ass@1.2.3.4:1080"); strings.Contains(got, "p:ass") {
		t.Errorf("password survived redaction: %q", got)
	}
	if got := redactProxyLine("1.2.3.4:1080 user:p:ass"); strings.Contains(got, "p:ass") {
		t.Errorf("password survived redaction: %q", got)
	}
	// Regression: a bare trailing '@' (user:pass@ with no host suffix) must
	// not leak the credential — return a placeholder instead.
	if got := redactProxyLine("user:secretpass@"); strings.Contains(got, "secretpass") {
		t.Errorf("redactProxyLine trailing @ leaked credential: %q", got)
	}
}

// TestSanitizeURLForDisplay: a source URL bearing an API token in the query
// must not echo the token to the status line.
func TestSanitizeURLForDisplay(t *testing.T) {
	out := sanitizeURLForDisplay("https://user:pass@api.provider.com/list?key=SECRET&x=1")
	for _, leak := range []string{"SECRET", "pass"} {
		if strings.Contains(out, leak) {
			t.Errorf("sanitizeURLForDisplay leaked %q: %q", leak, out)
		}
	}
	if strings.Contains(out, "?") {
		t.Errorf("sanitizeURLForDisplay kept query string: %q", out)
	}
	// Regression: a sensitive fragment must be cleared.
	out = sanitizeURLForDisplay("https://api.provider.com/list#token=SECRET")
	if strings.Contains(out, "SECRET") {
		t.Errorf("sanitizeURLForDisplay leaked fragment: %q", out)
	}
	if strings.Contains(out, "#") {
		t.Errorf("sanitizeURLForDisplay kept fragment: %q", out)
	}
	// Regression: malformed URL (parse failure) must return the safe
	// placeholder, not echo the raw input.
	bad := sanitizeURLForDisplay("ht!tp://[::1")
	if bad != "<invalid source URL>" {
		t.Errorf("sanitizeURLForDisplay malformed = %q, want placeholder", bad)
	}
}

// TestNormalizeProxyLinePseudoIPv6: a colon-containing non-IPv6 host must be
// rejected, not bracketed into a never-dialable pseudo-IPv6 address.
func TestNormalizeProxyLinePseudoIPv6(t *testing.T) {
	if got := normalizeProxyLine("host:1.2.3.4:1080"); got != "" {
		t.Errorf("normalizeProxyLine pseudo-IPv6 = %q, want empty", got)
	}
	if got := normalizeProxyLine("user@host:1.2.3.4:1080"); got != "" {
		t.Errorf("normalizeProxyLine cred-free pseudo-IPv6 = %q, want empty", got)
	}
}

func TestSetupPasteSignalHandler_NormalCleanup(t *testing.T) {
	called := false
	remove := func() { called = true }
	cleanup := setupPasteSignalHandler(remove)
	cleanup()
	if called {
		t.Errorf("removeTempFile should not be called on normal cleanup")
	}
}

func TestSetupPasteSignalHandler_SignalHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PASTE_SIGNAL_HELPER") != "1" {
		return
	}
	tmpFile, err := os.CreateTemp("", "proxy-paste-test-*.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp error: %v\n", err)
		os.Exit(2)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	remove := func() {
		_ = os.Remove(tmpPath)
	}
	_ = setupPasteSignalHandler(remove)

	// Print temp path to stdout so parent test knows file is ready
	fmt.Println(tmpPath)

	// Wait for signal
	select {
	case <-time.After(5 * time.Second):
		remove()
		os.Exit(3)
	}
}

func TestSetupPasteSignalHandler_RemovesTempFileOnSignal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSetupPasteSignalHandler_SignalHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_PASTE_SIGNAL_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("failed to read temp path from helper process: %v", scanner.Err())
	}
	tmpPath := scanner.Text()

	// Verify temp file exists
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("expected temp file to exist: %v", err)
	}

	// Send Interrupt signal to helper process
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("failed to send interrupt: %v", err)
	}

	_ = cmd.Wait()

	// Verify temp file was removed by the signal handler
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		_ = os.Remove(tmpPath)
		t.Errorf("expected temp file %s to be removed on signal, stat err: %v", tmpPath, err)
	}
}
