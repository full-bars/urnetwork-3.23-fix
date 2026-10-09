//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	winio "github.com/Microsoft/go-winio"
	"github.com/docopt/docopt-go"
	"golang.org/x/sys/windows"
)

// HotswapParentSession tracks the spawned candidate process and its named-pipe
// IPC channel on Windows.
type HotswapParentSession struct {
	childCmd     *exec.Cmd
	conn         net.Conn
	pipeListener net.Listener
	Reader       *bufio.Reader
	Writer       io.Writer

	waitOnce sync.Once
	waitErr  error
}

// Close closes the named-pipe connection and listener.
func (s *HotswapParentSession) Close() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.pipeListener != nil {
		_ = s.pipeListener.Close()
	}
}

// Kill terminates the candidate child process if it is still running.
func (s *HotswapParentSession) Kill() {
	if s.childCmd != nil && s.childCmd.Process != nil {
		_ = s.childCmd.Process.Kill()
		_ = s.Wait()
	}
	s.Close()
}

// hasChildProcess reports whether a real candidate process backs this session.
func (s *HotswapParentSession) hasChildProcess() bool {
	return s != nil && s.childCmd != nil
}

// Wait waits for the candidate child process to exit.
func (s *HotswapParentSession) Wait() error {
	if s.childCmd == nil {
		return nil
	}
	s.waitOnce.Do(func() {
		s.waitErr = s.childCmd.Wait()
	})
	return s.waitErr
}

// hotSwapPipeTimeout is the max duration the parent waits for the candidate to
// connect to the named pipe, and how long the child waits to dial.
const hotSwapPipeTimeout = 10 * time.Second

// randomPipeName generates a unique named-pipe path using 8 random hex bytes.
func randomPipeName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf(`\\.\pipe\urnetwork-hotswap-%x`, b)
}

// currentUserPipeSDDL returns a Windows SDDL security descriptor that grants
// pipe access only to the current user. This prevents other local users from
// reading the hot-swap IPC handshake on the named pipe.
func currentUserPipeSDDL() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("get token user: %w", err)
	}
	// D: = DACL, A;;FA;;;SID = Allow FILE_ALL_ACCESS to the given SID
	return "D:(A;;FA;;;" + user.User.Sid.String() + ")", nil
}

// spawnHotSwapCandidate creates a named pipe, spawns the candidate process with
// the pipe name in its environment, and waits for the candidate to connect.
func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	pipeName := randomPipeName()

	// Build a user-scoped pipe ACL. Default named-pipe security lets any local
	// user connect, which would expose the IPC handshake to other users.
	sddl, err := currentUserPipeSDDL()
	if err != nil {
		return nil, fmt.Errorf("build user-scoped pipe ACL: %w", err)
	}
	pipeConfig := &winio.PipeConfig{SecurityDescriptor: sddl}

	listener, err := winio.ListenPipe(pipeName, pipeConfig)
	if err != nil {
		return nil, fmt.Errorf("listen named pipe %s: %w", pipeName, err)
	}

	// Sanitize args: candidate must never re-run auth or re-authenticate
	// with old credentials (F-3).
	cleanArgs := sanitizeCandidateArgs(args)

	cmd := exec.Command(exe, cleanArgs...)
	// Strip stale URNETWORK_HOTSWAP_PIPE and URNETWORK_HOTSWAP from the
	// inherited environment to prevent chained hotswaps from reading an
	// obsolete pipe name. On Windows, GetEnvironmentVariable returns the
	// first definition, so leaving stale entries causes the candidate to
	// dial a dead pipe and abort (os.Exit 2).
	cleanEnv := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "URNETWORK_HOTSWAP_PIPE=") ||
			strings.HasPrefix(e, EnvHotSwap+"=") {
			continue
		}
		cleanEnv = append(cleanEnv, e)
	}
	cmd.Env = append(cleanEnv,
		EnvHotSwap+"=1",
		"URNETWORK_HOTSWAP_PIPE="+pipeName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("cmd.Start candidate: %w", err)
	}

	// Accept the candidate's pipe connection with a bounded timeout.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		acceptCh <- acceptResult{conn, err}
	}()

	select {
	case res := <-acceptCh:
		if res.err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = listener.Close()
			return nil, fmt.Errorf("accept named pipe: %w", res.err)
		}
		return &HotswapParentSession{
			childCmd:     cmd,
			conn:         res.conn,
			pipeListener: listener,
			Reader:       bufio.NewReader(res.conn),
			Writer:       res.conn,
		}, nil
	case <-time.After(hotSwapPipeTimeout):
		_ = listener.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("timed out waiting for candidate to connect to named pipe (%s)", hotSwapPipeTimeout)
	}
}

// getHotSwapChildIPC connects to the parent's named pipe if this process was
// launched as a hot-swap candidate on Windows. It reads the pipe name from the
// URNETWORK_HOTSWAP_PIPE environment variable.
func getHotSwapChildIPC() (io.ReadWriteCloser, bool) {
	if os.Getenv(EnvHotSwap) != "1" {
		return nil, false
	}

	pipeName := os.Getenv("URNETWORK_HOTSWAP_PIPE")
	if pipeName == "" {
		tlog("❌ [hotswap] %s=1 is set but URNETWORK_HOTSWAP_PIPE is empty — cannot connect to parent\n", EnvHotSwap)
		os.Exit(2) // Same as DialPipe failure: do NOT fall back to normal provider.
	}

	timeout := hotSwapPipeTimeout
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		tlog("❌ [hotswap] Failed to connect to named pipe %s: %v\n", pipeName, err)
		os.Exit(2) // Do NOT fall back to normal provider — that causes split-brain.
	}

	return conn, true
}

// startHotSwapSignalListener is a no-op on Windows. Named-pipe based hot-swap
// triggers will be wired in a subsequent PR.
func startHotSwapSignalListener(ctx context.Context, cancel context.CancelFunc, opts docopt.Opts) {
	// No-op on Windows — no signal-based hot-swap trigger yet.
}

// notifySystemdMainPID is a no-op on Windows (no systemd).
func notifySystemdMainPID(pid int) error { return nil }

// notifySystemdReady is a no-op on Windows (no systemd).
func notifySystemdReady() error    { return nil }
func notifySystemdWatchdog() error { return nil }

// notifySystemdStatus is a no-op on Windows (no systemd).
func notifySystemdStatus(text string) error { return nil }

// execInPlace is not supported on Windows; the container PID-1 branch of
// hotswap is Linux-only.
func execInPlace(exe string, args []string, env []string) error {
	return errors.New("execve not supported on Windows")
}

// checkExecAccess is a no-op stub on Windows. checkExecutable skips the POSIX
// access check entirely for GOOS == "windows".
func checkExecAccess(path string) error { return nil }
