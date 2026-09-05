//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

func TestHotSwapPreflightValidation(t *testing.T) {
	// Setup isolated HOME
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	if err := os.MkdirAll(urDir, 0700); err != nil {
		t.Fatal(err)
	}

	// 1. Missing JWT should fail
	dummyOpts := docopt.Opts{}
	err := runHotSwapChildPreflight(dummyOpts, "https://127.0.0.1:443")
	if err == nil {
		t.Errorf("expected error on missing JWT, got nil")
	}

	// 2. Expired JWT should fail
	expiredToken := createFakeJWT(time.Now().Add(-1 * time.Hour).Unix())
	jwtPath := filepath.Join(urDir, "jwt")
	if err := os.WriteFile(jwtPath, []byte(expiredToken), 0600); err != nil {
		t.Fatal(err)
	}

	err = runHotSwapChildPreflight(dummyOpts, "https://127.0.0.1:443")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("expired")) {
		t.Errorf("expected token expired error, got: %v", err)
	}

	// 3. Valid future JWT with local listener should pass
	validToken := createFakeJWT(time.Now().Add(24 * time.Hour).Unix())
	if err := os.WriteFile(jwtPath, []byte(validToken), 0600); err != nil {
		t.Fatal(err)
	}

	// Start a mock API listener to verify dial
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	mockApiUrl := "http://" + listener.Addr().String()
	err = runHotSwapChildPreflight(dummyOpts, mockApiUrl)
	if err != nil {
		t.Errorf("expected valid preflight to succeed, got: %v", err)
	}
}

func TestHotSwapHandshakeSuccess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	// Setup isolated HOME with valid JWT
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeJWT(time.Now().Add(48 * time.Hour).Unix())
	_ = os.WriteFile(filepath.Join(urDir, "jwt"), []byte(validToken), 0600)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	childDone := make(chan error, 1)

	// Run child handshake in goroutine
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, mockApiUrl)
	}()

	// Parent side verification
	parentReader := bufio.NewReader(parentSock)

	// 1. Parent reads READY
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read READY: %v", err)
	}
	if msg.Type != HotswapMsgReady {
		t.Fatalf("expected READY, got %s (err=%s)", msg.Type, msg.Error)
	}

	// 2. Parent sends TAKEOVER
	if err := writeHotswapMessage(parentSock, HotswapMessage{
		Type: HotswapMsgTakeover,
		PID:  os.Getpid(),
	}); err != nil {
		t.Fatalf("parent send TAKEOVER: %v", err)
	}

	// 3. Child completes handshake
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("child handshake failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child handshake")
	}

	// 4. Child sends ACK
	if err := runHotSwapChildAck(childSock); err != nil {
		t.Fatalf("child send ACK: %v", err)
	}

	ackMsg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read ACK: %v", err)
	}
	if ackMsg.Type != HotswapMsgAck {
		t.Fatalf("expected ACK, got %s", ackMsg.Type)
	}
}

func TestHotSwapHandshakeChildFailure(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	// Empty HOME with no JWT should trigger ERROR message
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	childDone := make(chan error, 1)
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, "https://127.0.0.1:443")
	}()

	parentReader := bufio.NewReader(parentSock)
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read message: %v", err)
	}
	if msg.Type != HotswapMsgError {
		t.Fatalf("expected ERROR message, got %s", msg.Type)
	}
	if msg.Error == "" {
		t.Errorf("expected error message content, got empty string")
	}

	select {
	case err := <-childDone:
		if err == nil {
			t.Fatal("expected child handshake to return error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child handshake to fail")
	}
}

func TestYieldCoordinatorSession(t *testing.T) {
	ClearCoordinatorClosers()
	called1 := false
	called2 := false
	RegisterCoordinatorCloser(func() {
		called1 = true
	})
	RegisterCoordinatorCloser(func() {
		called2 = true
	})

	yieldCoordinatorSession()

	if !called1 || !called2 {
		t.Errorf("expected all coordinator closers to be called: called1=%v called2=%v", called1, called2)
	}

	// Clearing closers
	ClearCoordinatorClosers()
	called1 = false
	called2 = false
	yieldCoordinatorSession()
	if called1 || called2 {
		t.Errorf("expected no closers to be called after clearing")
	}
}

func TestNotifySystemdMainPID(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "notify.sock")

	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer l.Close()

	origNotify := os.Getenv("NOTIFY_SOCKET")
	defer os.Setenv("NOTIFY_SOCKET", origNotify)
	os.Setenv("NOTIFY_SOCKET", sockPath)

	if err := notifySystemdMainPID(45678); err != nil {
		t.Fatalf("notifySystemdMainPID: %v", err)
	}

	buf := make([]byte, 128)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}

	got := string(buf[:n])
	want := "MAINPID=45678\n"
	if got != want {
		t.Errorf("got notify payload %q, want %q", got, want)
	}
}

func TestHotSwapCanaryHandshake(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeJWT(time.Now().Add(48 * time.Hour).Unix())
	_ = os.WriteFile(filepath.Join(urDir, "jwt"), []byte(validToken), 0600)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	origExit := exitFunc
	exitCalled := make(chan int, 1)
	exitFunc = func(code int) {
		exitCalled <- code
	}
	defer func() { exitFunc = origExit }()

	childDone := make(chan error, 1)
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, mockApiUrl)
	}()

	parentReader := bufio.NewReader(parentSock)
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read READY: %v", err)
	}
	if msg.Type != HotswapMsgReady {
		t.Fatalf("expected READY, got %s", msg.Type)
	}

	// Send CANARY_DONE from parent
	if err := writeHotswapMessage(parentSock, HotswapMessage{
		Type: HotswapMsgCanaryDone,
		PID:  1,
	}); err != nil {
		t.Fatalf("parent send CANARY_DONE: %v", err)
	}

	select {
	case code := <-exitCalled:
		if code != 0 {
			t.Errorf("expected exit code 0 on CANARY_DONE, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canary exit on CANARY_DONE")
	}
}

func TestHotSwapParentPID1ExecveSuccess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origExecInPlace := execInPlaceFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		execInPlaceFunc = origExecInPlace
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 1 } // simulate Docker PID 1

	execCalled := make(chan struct{}, 1)
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		// Verify URNETWORK_HOTSWAP is stripped from cleanEnv
		for _, e := range env {
			if e == EnvHotSwap+"=1" {
				t.Errorf("cleanEnv should not contain %s", EnvHotSwap)
			}
		}
		execCalled <- struct{}{}
		return nil
	}

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})

	// Simulate candidate announcing READY on childFile
	go func() {
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     2,
			Version: "v3.23.0-fix.31.0",
		})
		// Candidate waits for CANARY_DONE
		resp, _ := readHotswapMessage(childReader)
		if resp.Type != HotswapMsgCanaryDone {
			t.Errorf("candidate expected CANARY_DONE, got %s", resp.Type)
		}
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err != nil {
		t.Fatalf("runHotSwapParentHandoff returned error: %v", err)
	}

	select {
	case <-execCalled:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-place execve call")
	}

	if !coordinatorYielded {
		t.Errorf("expected coordinator session to be yielded before execve")
	}
}

func TestHotSwapParentPID1PreflightFailurePreservesProcess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origExecInPlace := execInPlaceFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		execInPlaceFunc = origExecInPlace
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 1 }

	execCalled := false
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		execCalled = true
		return nil
	}

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})

	// Simulate candidate reporting ERROR
	go func() {
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:  HotswapMsgError,
			PID:   2,
			Error: "simulated pre-flight failure",
		})
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil {
		t.Fatal("expected error on candidate preflight failure, got nil")
	}

	if execCalled {
		t.Errorf("execve must NOT be called on preflight failure")
	}
	if coordinatorYielded {
		t.Errorf("coordinator must NOT be yielded on preflight failure")
	}
}

func TestHotSwapPreflightPermissionHealing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission healing is unobservable as root (CAP_DAC_OVERRIDE)")
	}

	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeJWT(time.Now().Add(48 * time.Hour).Unix())
	jwtPath := filepath.Join(urDir, "jwt")
	_ = os.WriteFile(jwtPath, []byte(validToken), 0600)

	// Restrict permissions to unreadable
	_ = os.Chmod(jwtPath, 0000)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	err = runHotSwapChildPreflight(docopt.Opts{}, mockApiUrl)
	if err != nil {
		t.Fatalf("expected preflight to heal permissions and succeed, got: %v", err)
	}

	fi, err := os.Stat(jwtPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("expected healed permissions 0600, got %v", fi.Mode().Perm())
	}
}

func TestHotSwapParentStandardBatonSuccess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	origGetpid := getpidFunc
	origSpawn := spawnCandidateFunc
	origExit := exitFunc
	defer func() {
		getpidFunc = origGetpid
		spawnCandidateFunc = origSpawn
		exitFunc = origExit
	}()

	getpidFunc = func() int { return 9999 } // Host non-PID-1
	parentExited := make(chan int, 1)
	exitFunc = func(code int) { parentExited <- code }

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	unreg := RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})
	defer unreg()

	// Child simulation: announce READY -> wait TAKEOVER -> send ACK
	childDone := make(chan struct{})
	go func() {
		defer close(childDone)
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     10000,
			Version: "v3.23.0-fix.31.0",
		})

		msg, err := readHotswapMessage(childReader)
		if err != nil || msg.Type != HotswapMsgTakeover {
			t.Errorf("child expected TAKEOVER, got %s (err %v)", msg.Type, err)
			return
		}

		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type: HotswapMsgAck,
			PID:  10000,
		})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = runHotSwapParentHandoff(ctx, cancel, docopt.Opts{})
	if err != nil {
		t.Fatalf("runHotSwapParentHandoff returned error: %v", err)
	}

	<-childDone

	// Cancel context to complete drain sleep immediately in test
	cancel()
	select {
	case code := <-parentExited:
		if code != 0 {
			t.Errorf("expected exit code 0 on drain, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for parent exit on drain")
	}

	if !coordinatorYielded {
		t.Errorf("expected coordinator session to be yielded during handoff")
	}
}

func TestHotSwapParentStandardBatonAckTimeoutAborts(t *testing.T) {
	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 9999 }

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	// Child announces READY, receives TAKEOVER, but closes connection post-TAKEOVER (crash)
	go func() {
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     10000,
			Version: "v3.23.0-fix.31.0",
		})
		_, _ = readHotswapMessage(childReader)
		// Close childFile to simulate candidate crash post-TAKEOVER
		childFile.Close()
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil {
		t.Fatal("expected error on candidate unconfirmed takeover, got nil")
	}
}

func TestRegisterCoordinatorCloserUnregister(t *testing.T) {
	ClearCoordinatorClosers()

	called := false
	unreg := RegisterCoordinatorCloser(func() {
		called = true
	})

	// Before unregister, yield calls closer
	yieldCoordinatorSession()
	if !called {
		t.Fatalf("closer was not called")
	}

	// Unregister
	unreg()
	called = false

	// After unregister, closer is not called
	yieldCoordinatorSession()
	if called {
		t.Fatalf("unregistered closer was unexpectedly called (memory leak)")
	}
}

func TestGetHotSwapChildIPCValidation(t *testing.T) {
	origEnv := os.Getenv(EnvHotSwap)
	defer os.Setenv(EnvHotSwap, origEnv)

	// If env var is not set, returns false
	os.Unsetenv(EnvHotSwap)
	if _, isChild := getHotSwapChildIPC(); isChild {
		t.Errorf("expected isChild=false when %s is unset", EnvHotSwap)
	}

	// If env var is set but fd 3 is not a socket, returns false (F-11)
	os.Setenv(EnvHotSwap, "1")
	// On arbitrary process fd 3 is typically either closed or not a socket
	_ = syscall.Close(3)
	if _, isChild := getHotSwapChildIPC(); isChild {
		t.Errorf("expected isChild=false when fd 3 is invalid/closed")
	}
}

func TestSanitizeCandidateArgs(t *testing.T) {
	// auth-provide with positional auth code should become pure "provide"
	got := sanitizeCandidateArgs([]string{"auth-provide", "abc123secret", "--user"})
	want := []string{"provide", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth-provide + code: got %v, want %v", got, want)
	}

	// auth-provide with flag-style next arg should not skip it
	got = sanitizeCandidateArgs([]string{"auth-provide", "-f", "--user"})
	want = []string{"provide", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth-provide + flags: got %v, want %v", got, want)
	}

	// -f flag stripped
	got = sanitizeCandidateArgs([]string{"provide", "-f", "--port", "8080"})
	want = []string{"provide", "--port", "8080"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("-f strip: got %v, want %v", got, want)
	}

	// --user_auth and --password flags stripped, including their positional values (separated form)
	got = sanitizeCandidateArgs([]string{"provide", "--user_auth", "admin", "--password", "secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth args strip: got %v, want %v", got, want)
	}

	// --user_auth=val form (value attached) is fully stripped
	got = sanitizeCandidateArgs([]string{"provide", "--user_auth=admin", "--password=secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attached auth args strip: got %v, want %v", got, want)
	}

	// Full auth-provide invocation sanitized to pure provide
	got = sanitizeCandidateArgs([]string{"auth-provide", "ABC123secret", "-f", "--user_auth", "admin", "--password", "secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("full auth-provide sanitize: got %v, want %v", got, want)
	}

	// Normal args pass through unchanged
	got = sanitizeCandidateArgs([]string{"provide", "--port", "8080", "--user"})
	want = []string{"provide", "--port", "8080", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("passthrough: got %v, want %v", got, want)
	}
}

func TestClientJWTStoreFlockExclusivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	// First store acquires lock and writes
	if err := store.Put("proxy-1", clientJWTEntry{
		ByClientJWT: "jwt1",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("Put 1 failed: %v", err)
	}

	// Second store instance acquires its own lock (sequential, not concurrent in same process
	// but verifies the lock file mechanism works and doesn't block)
	store2 := newClientJWTStore(path)
	if err := store2.Put("proxy-2", clientJWTEntry{
		ByClientJWT: "jwt2",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("Put 2 (flock acquire) failed: %v", err)
	}

	// Both entries should be present (store2 loaded store1's data + added its own)
	got, ok := store2.Get("proxy-1")
	if !ok {
		t.Error("expected proxy-1 entry to survive after store2 Put")
	}
	if got.ByClientJWT != "jwt1" {
		t.Errorf("proxy-1 JWT = %q, want jwt1 (stale-map overwrite would have lost this)", got.ByClientJWT)
	}
}



