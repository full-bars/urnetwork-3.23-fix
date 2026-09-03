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
		return strings.Contains(s, "providers found") || strings.Contains(s, "no providers") || strings.Contains(s, "[dry-run]")
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
}
