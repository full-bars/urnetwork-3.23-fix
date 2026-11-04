package urnettools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDiagProvider listens on <dir>/provider.sock and answers every request
// with reply, after delay. The directory is made under /tmp because a unix
// socket path is limited to about 100 bytes and t.TempDir() can exceed it.
func fakeDiagProvider(t *testing.T, reply controlResponse, delay time.Duration) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix socket")
	}
	dir, err := os.MkdirTemp("/tmp", "dg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "provider.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				var req controlRequest
				json.Unmarshal(line, &req)
				if req.Cmd != "diag" {
					json.NewEncoder(c).Encode(controlResponse{Error: "unknown command " + req.Cmd})
					return
				}
				time.Sleep(delay)
				json.NewEncoder(c).Encode(reply)
			}()
		}
	}()
	return dir
}

func okDiagReply() controlResponse {
	b, _ := json.Marshal(diagReply{
		Dir:   "/home/u/.urnetwork/diag/20261103T202500Z",
		Files: []string{"summary.txt", "heap.pb.gz", "goroutine.txt", "cpu.pb.gz"},
		Notes: []string{"a note"}, RSSBytes: 4952 << 20, HeapInuse: 3 << 30, HeapSys: 4 << 30,
		Goroutines: 91234, UptimeSeconds: 39720, Clients: 12,
	})
	return controlResponse{OK: true, Value: string(b)}
}

func runDiagForTest(t *testing.T, args []string, p Provider) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	err = runDiagCmd(args, []Provider{p}, &out, &errb)
	return out.String(), errb.String(), err
}

func TestDiagCmdPrintsPathAndHeadlines(t *testing.T) {
	dir := fakeDiagProvider(t, okDiagReply(), 0)
	out, progress, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err != nil {
		t.Fatalf("diag: %v", err)
	}
	for _, want := range []string{
		"Diagnostic bundle: /home/u/.urnetwork/diag/20261103T202500Z",
		"memory (RSS) 4.8 GiB", "heap in use 3.0 GiB (after GC)", "heap reserved 4.0 GiB",
		"goroutines 91234", "clients 12", "uptime 11h2m0s",
		"files: summary.txt, heap.pb.gz, goroutine.txt, cpu.pb.gz", "note: a note",
		"Read summary.txt first", "session tokens",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(progress, "not restarted") || !strings.Contains(progress, "about 40 seconds") {
		t.Errorf("progress line should say it waits and does not restart: %q", progress)
	}
}

func TestDiagCmdRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"--json"}, {"now"}, {"--interval", "5s"}} {
		_, _, err := runDiagForTest(t, args, Provider{StateDir: "/x", Running: true})
		if err == nil || !strings.Contains(err.Error(), "takes no flags or arguments") {
			t.Errorf("args %v: err = %v, want the no-arguments refusal", args, err)
		}
	}
}

func TestDiagCmdProviderNotRunning(t *testing.T) {
	_, progress, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: "/x", Running: false})
	if err == nil {
		t.Fatal("want an error for a stopped provider")
	}
	for _, want := range []string{"is not running", "nothing to capture", "urnet-tools start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if progress != "" {
		t.Errorf("printed a progress line for a capture that never started: %q", progress)
	}
}

func TestDiagCmdSocketMissing(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "dg")
	t.Cleanup(func() { os.RemoveAll(dir) })
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil {
		t.Fatal("want an error with no control socket")
	}
	out, progress, _ := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if out != "" || progress != "" {
		t.Errorf("announced a capture that could not start: %q %q", out, progress)
	}
	for _, want := range []string{"control socket " + filepath.Join(dir, "provider.sock") + " is not available", "urnet-tools status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestDiagCmdSocketNotPermitted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open any socket")
	}
	dir := fakeDiagProvider(t, okDiagReply(), 0)
	sock := filepath.Join(dir, "provider.sock")
	if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil || !strings.Contains(err.Error(), "owner-only") || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v, want a permission message that says who to run it as", err)
	}
}

func TestDiagCmdOldProvider(t *testing.T) {
	dir := fakeDiagProvider(t, controlResponse{Error: `unknown command "diag"`}, 0)
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil || !strings.Contains(err.Error(), "too old to know `diag`") || !strings.Contains(err.Error(), "urnet-tools update") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagCmdPassesProviderRefusalThrough(t *testing.T) {
	msg := "diag: not enough free space on the filesystem holding /x/diag: need 320.0 MiB, have 100.0 MiB, short by 220.0 MiB. Free some space and run it again"
	dir := fakeDiagProvider(t, controlResponse{Error: msg}, 0)
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil || !strings.Contains(err.Error(), "short by 220.0 MiB") || strings.HasPrefix(err.Error(), "diag: ") {
		t.Fatalf("err = %v, want the provider's message with its prefix dropped", err)
	}
}

func TestDiagCmdProviderDoesNotAnswer(t *testing.T) {
	prev := diagClientTimeout
	diagClientTimeout = 150 * time.Millisecond
	t.Cleanup(func() { diagClientTimeout = prev })
	dir := fakeDiagProvider(t, okDiagReply(), 2*time.Second)
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil || !strings.Contains(err.Error(), "did not answer within") || !strings.Contains(err.Error(), filepath.Join(dir, "diag")) {
		t.Fatalf("err = %v, want a timeout that names where to look", err)
	}
}

func TestDiagCmdUnreadableReply(t *testing.T) {
	dir := fakeDiagProvider(t, controlResponse{OK: true, Value: "not json"}, 0)
	_, _, err := runDiagForTest(t, nil, Provider{Unit: "urnetwork.service", StateDir: dir, Running: true})
	if err == nil || !strings.Contains(err.Error(), "different versions") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagCmdIsRegistered(t *testing.T) {
	var found bool
	for _, c := range buildRootCmd().Commands() {
		if c.Name() == "diag" {
			found = true
		}
	}
	if !found {
		t.Fatal("diag is not registered on the root command")
	}
}
