//go:build linux

package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPeerVersionRealSocket(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	startVersionMockServer(t, filepath.Join(dir, "provider.sock"), controlResponse{OK: true, BuildVersion: "v-new"})
	if got, ok := providerVersionFromSocketForPID(p, os.Getpid()); !ok || got != "v-new" {
		t.Fatalf("own listener: %q %v", got, ok)
	}
	if got, ok := providerVersionFromSocketForPID(p, os.Getpid()+1000000); ok {
		t.Fatalf("wrong pid accepted: %q", got)
	}
	if got, ok := providerVersionFromSocketForPID(Provider{StateDir: t.TempDir()}, os.Getpid()); ok {
		t.Fatalf("missing socket accepted: %q", got)
	}
}

// A separate process plants a listener that really answers the target version.
func TestPeerVersionPlantedListener(t *testing.T) {
	if dir := os.Getenv("TEST_PEER_CHILD_DIR"); dir != "" {
		l, err := net.Listen("unix", filepath.Join(dir, "provider.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		fmt.Println("ready")
		for i := 0; i < 2; i++ {
			c, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			var req controlRequest
			if json.NewDecoder(c).Decode(&req) == nil {
				_ = json.NewEncoder(c).Encode(controlResponse{OK: true, BuildVersion: "v-new"})
			}
			c.Close()
		}
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPeerVersionPlantedListener$")
	cmd.Env = append(os.Environ(), "TEST_PEER_CHILD_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("child readiness %q: %v", line, err)
	}
	p := Provider{StateDir: dir}
	if v, ok := providerVersionFromSocket(p); !ok || v != "v-new" {
		t.Fatalf("planted listener did not answer: %q %v", v, ok)
	}
	if v, ok := providerVersionFromSocketForPID(p, os.Getpid()); ok {
		t.Fatalf("planted listener accepted: %q", v)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
