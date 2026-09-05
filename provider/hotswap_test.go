package main

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

func TestHotSwapMessageFraming(t *testing.T) {
	var buf bytes.Buffer

	want := HotswapMessage{
		Type:    HotswapMsgReady,
		Version: "v3.23.0-fix.31.0",
		PID:     12345,
	}

	if err := writeHotswapMessage(&buf, want); err != nil {
		t.Fatalf("writeHotswapMessage: %v", err)
	}

	reader := bufio.NewReader(&buf)
	got, err := readHotswapMessage(reader)
	if err != nil {
		t.Fatalf("readHotswapMessage: %v", err)
	}

	if got.Type != want.Type {
		t.Errorf("got.Type = %v, want %v", got.Type, want.Type)
	}
	if got.Version != want.Version {
		t.Errorf("got.Version = %v, want %v", got.Version, want.Version)
	}
	if got.PID != want.PID {
		t.Errorf("got.PID = %v, want %v", got.PID, want.PID)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("got.Timestamp is zero")
	}
}

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

