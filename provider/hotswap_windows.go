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

// spawnHotSwapCandidate creates a named pipe, spawns the candidate process with
// the pipe name in its environment, and waits for the candidate to connect.
func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	pipeName := randomPipeName()

	// Listen on named pipe. nil PipeConfig uses default security: the creating
	// user gets full access, SYSTEM/Admins also have access. For tighter ACLs
	// a SecurityDescriptor in SDDL format can be set here.
	listener, err := winio.ListenPipe(pipeName, nil)
	if err != nil {
		return nil, fmt.Errorf("listen named pipe %s: %w", pipeName, err)
	}

	// Sanitize args: candidate must never re-run auth or re-authenticate
	// with old credentials (F-3).
	cleanArgs := sanitizeCandidateArgs(args)

	cmd := exec.Command(exe, cleanArgs...)
	cmd.Env = append(os.Environ(),
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
		tlog("⚠️ [hotswap] %s=1 is set but URNETWORK_HOTSWAP_PIPE is empty; starting as normal provider\n", EnvHotSwap)
		return nil, false
	}

	timeout := hotSwapPipeTimeout
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		tlog("⚠️ [hotswap] Failed to connect to named pipe %s: %v; starting as normal provider\n", pipeName, err)
		return nil, false
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
func notifySystemdReady() error { return nil }

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

// sanitizeCandidateArgs strips identity-mutating arguments from the parent's
// argv so the HotSwap candidate never re-authenticates (F-3).
// Duplicated from hotswap_unix.go for Windows compilation.
func sanitizeCandidateArgs(args []string) []string {
	var cleanArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "auth-provide" {
			cleanArgs = append(cleanArgs, "provide")
			// If followed by positional auth code, skip it
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if arg == "-f" {
			continue
		}
		if arg == "--user_auth" {
			i++ // skip flag + value
			continue
		}
		if arg == "--password" {
			i++ // skip flag + value
			continue
		}
		if strings.HasPrefix(arg, "--user_auth=") || strings.HasPrefix(arg, "--password=") {
			continue
		}
		cleanArgs = append(cleanArgs, arg)
	}
	return cleanArgs
}
