package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerDirectUsageCommandBuilders ensures the new commands are registered and executable.
func TestDockerDirectUsageCommandBuilders(t *testing.T) {
	root := buildDockerRootCmd()
	cmds := map[string]bool{}
	for _, c := range root.Commands() {
		cmds[c.Name()] = true
	}
	for _, expected := range []string{"direct", "usage", "proxy"} {
		if !cmds[expected] {
			t.Errorf("buildDockerRootCmd missing %q command", expected)
		}
	}
}

// TestDockerProxySubcommands includes paste in the subcommand list.
func TestDockerProxySubcommands(t *testing.T) {
	proxyCmd := newDockerProxyCmd()
	subMap := map[string]bool{}
	for _, c := range proxyCmd.Commands() {
		subMap[c.Name()] = true
	}
	for _, expected := range []string{"add", "paste", "clear", "remove", "refresh", "health", "traffic", "summary"} {
		if !subMap[expected] {
			t.Errorf("newDockerProxyCmd missing subcommand %q", expected)
		}
	}
}

// TestProxyAddFlagAndUrlParsing covers --file, --proxy_file, --url, and straight path handling.
func TestProxyAddFlagAndUrlParsing(t *testing.T) {
	tmpDir := t.TempDir()
	proxyFile := filepath.Join(tmpDir, "proxies.txt")
	if err := os.WriteFile(proxyFile, []byte("127.0.0.1:1080\n"), 0600); err != nil {
		t.Fatal(err)
	}

	isAcceptable := func(err error) bool {
		if err == nil {
			return true
		}
		s := err.Error()
		return strings.Contains(s, "providers found") ||
			strings.Contains(s, "no providers") ||
			strings.Contains(s, "matches no provider") ||
			strings.Contains(s, "[dry-run]")
	}

	// 1. Straight path
	t.Run("straight-path", func(t *testing.T) {
		err := Run([]string{"proxy", "add", proxyFile, "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// 2. --file= flag
	t.Run("file-flag", func(t *testing.T) {
		err := Run([]string{"proxy", "add", "--file=" + proxyFile, "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error with --file=: %v", err)
		}
	})

	// 3. --proxy_file= flag
	t.Run("proxy-file-flag", func(t *testing.T) {
		err := Run([]string{"proxy", "add", "--proxy_file=" + proxyFile, "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error with --proxy_file=: %v", err)
		}
	})

	// 4. URL auto-routing
	t.Run("url-auto-route", func(t *testing.T) {
		err := Run([]string{"proxy", "add", "https://example.com/proxies.txt", "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error with URL auto-routing: %v", err)
		}
	})

	// 5. --url= flag
	t.Run("url-flag", func(t *testing.T) {
		err := Run([]string{"proxy", "add", "--url=https://example.com/proxies.txt", "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error with --url=: %v", err)
		}
	})

	// 6. Multiple straight files
	t.Run("multi-file-straight", func(t *testing.T) {
		proxyFile2 := filepath.Join(tmpDir, "proxies2.txt")
		if err := os.WriteFile(proxyFile2, []byte("127.0.0.2:1080\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := Run([]string{"proxy", "add", proxyFile, proxyFile2, "--dry-run", "--unit=urnetwork.service"})
		if !isAcceptable(err) {
			t.Fatalf("unexpected error with multi-file straight add: %v", err)
		}
	})
}

func TestExpandHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("skipping home expansion test: home unavailable")
	}

	got := expandHomePath("~/test/path.txt")
	expected := filepath.Join(home, "test/path.txt")
	if got != expected {
		t.Errorf("expandHomePath(~/...) = %q, want %q", got, expected)
	}

	gotWin := expandHomePath("~\\test\\path.txt")
	expectedWin := filepath.Join(home, "test\\path.txt")
	if gotWin != expectedWin {
		t.Errorf("expandHomePath(~\\...) = %q, want %q", gotWin, expectedWin)
	}

	gotLiteral := expandHomePath("/var/log/test.txt")
	if gotLiteral != "/var/log/test.txt" {
		t.Errorf("expandHomePath(/var/...) = %q, want /var/log/test.txt", gotLiteral)
	}
}
