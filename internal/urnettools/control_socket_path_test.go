package urnettools

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The provider listens on <state dir>/provider.sock. Every client path must
// name that file. Two commands added with the v31 CLI surface hardcoded
// "control.sock" instead, so they could never reach a running provider:
// they failed with "connect: no such file or directory" on a perfectly
// healthy node.
//
// This guards the whole package rather than the two call sites, because the
// defect is a literal that is easy to reintroduce and impossible to catch by
// reading one function.
func TestNoClientDialsControlSock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"control.sock"`) {
			t.Errorf("%s references \"control.sock\"; the provider listens on provider.sock", name)
		}
	}
}

// TestMetricsCommandReachesProviderSocket proves the fix end to end: a
// listener at provider.sock receives the request the metrics command sends.
// Without the fix the command dials control.sock and never arrives.
func TestMetricsCommandReachesProviderSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "provider.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
		_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
	}()

	p := Provider{StateDir: dir}
	if _, err := sendMetricsToggle(p, "on"); err != nil {
		t.Fatalf("metrics toggle: %v", err)
	}

	select {
	case line := <-got:
		if !strings.Contains(line, `"metrics"`) {
			t.Fatalf("provider received %q, want a metrics set request", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("provider socket never received the request")
	}
}
