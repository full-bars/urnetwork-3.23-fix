//go:build !windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
)

// HotswapParentSession tracks the spawned candidate process and its IPC channel.
type HotswapParentSession struct {
	childCmd *exec.Cmd
	parentFd *os.File
	Reader   *bufio.Reader
	Writer   io.Writer

	waitOnce sync.Once
	waitErr  error
}

// Close closes the parent end of the IPC socketpair.
func (s *HotswapParentSession) Close() {
	if s.parentFd != nil {
		_ = s.parentFd.Close()
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
// Wait returns immediately when one does not, which a liveness monitor would
// otherwise read as "the candidate died".
func (s *HotswapParentSession) hasChildProcess() bool {
	return s != nil && s.childCmd != nil
}

// Wait waits for the candidate child process to exit. Safe to call from
// multiple goroutines concurrently (e.g. a drain-timeout Kill racing a
// liveness-monitor Wait) — only the first caller reaches exec.Cmd.Wait,
// since os/exec does not support concurrent Wait calls on the same Cmd.
func (s *HotswapParentSession) Wait() error {
	if s.childCmd == nil {
		return nil
	}
	s.waitOnce.Do(func() {
		s.waitErr = s.childCmd.Wait()
	})
	return s.waitErr
}

// sanitizeCandidateArgs strips identity-mutating arguments from the parent's
// argv so the HotSwap candidate never re-authenticates or re-executes
// auth-provide with a stale auth code (F-3). Returns the cleaned argument list.
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
			// Skip the flag and its positional value (separated form: --user_auth <val>)
			i++
			continue
		}
		if arg == "--password" {
			// Skip the flag and its positional value (separated form: --password <val>)
			i++
			continue
		}
		if strings.HasPrefix(arg, "--user_auth=") || strings.HasPrefix(arg, "--password=") {
			continue
		}
		cleanArgs = append(cleanArgs, arg)
	}
	return cleanArgs
}

// spawnHotSwapCandidate creates an anonymous close-on-exec socketpair,
// passes descriptor 3 to the child via ExtraFiles, and starts the candidate process.
func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	// Close-on-exec prevents descriptor leakage into the child or unrelated
	// forks (F-7). How that is achieved is platform-specific: see
	// hotSwapSocketpair in hotswap_socketpair_linux.go (atomic SOCK_CLOEXEC)
	// and hotswap_socketpair_other.go (ForkLock + CloseOnExec, since darwin
	// and the BSDs have no SOCK_CLOEXEC).
	fds, err := hotSwapSocketpair()
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "hotswap-parent")
	childFile := os.NewFile(uintptr(fds[1]), "hotswap-child")
	defer childFile.Close()

	// Sanitize args: candidate must never re-run auth or re-authenticate with old credentials (F-3)
	cleanArgs := sanitizeCandidateArgs(args)

	cmd := exec.Command(exe, cleanArgs...)
	cmd.Env = append(os.Environ(), EnvHotSwap+"=1")
	// ExtraFiles[0] becomes file descriptor 3 in the child process
	cmd.ExtraFiles = []*os.File{childFile}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		// Self-healing: if binary lacks execute permission, repair with chmod 0755 and retry
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			tlog("⚠️ [hotswap-heal] Candidate binary %s missing execute permission; auto-healing chmod 0755...\n", exe)
			if chmodErr := os.Chmod(exe, 0755); chmodErr == nil {
				cmd = exec.Command(exe, cleanArgs...)
				cmd.Env = append(os.Environ(), EnvHotSwap+"=1")
				cmd.ExtraFiles = []*os.File{childFile}
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if retryErr := cmd.Start(); retryErr == nil {
					tlog("⚡ [hotswap-heal] Successfully spawned candidate after permission repair (PID %d)\n", cmd.Process.Pid)
					return &HotswapParentSession{
						childCmd: cmd,
						parentFd: parentFile,
						Reader:   bufio.NewReader(parentFile),
						Writer:   parentFile,
					}, nil
				}
			}
		}
		parentFile.Close()
		return nil, fmt.Errorf("cmd.Start candidate: %w", err)
	}

	return &HotswapParentSession{
		childCmd: cmd,
		parentFd: parentFile,
		Reader:   bufio.NewReader(parentFile),
		Writer:   parentFile,
	}, nil
}

// checkExecAccess reports whether the calling process can execute the file at
// path, using syscall.Access with X_OK (1) which checks execute permission
// according to the caller's real UID/GID.
func checkExecAccess(path string) error {
	if err := syscall.Access(path, 1); err != nil { // X_OK = 1
		return fmt.Errorf("access %s: %w", path, err)
	}
	return nil
}

// getHotSwapChildIPC returns the open IPC file descriptor (fd 3) if this process was launched as a candidate.
// It verifies that fd 3 is open and is a valid Unix domain socket before claiming candidate mode (F-11).
func getHotSwapChildIPC() (*os.File, bool) {
	if os.Getenv(EnvHotSwap) != "1" {
		return nil, false
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(3, &stat); err != nil {
		tlog("⚠️ [hotswap] %s=1 is set but descriptor 3 is invalid (%v); starting as normal provider\n", EnvHotSwap, err)
		return nil, false
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		tlog("⚠️ [hotswap] %s=1 is set but descriptor 3 is not a socket; starting as normal provider\n", EnvHotSwap)
		return nil, false
	}

	// Go's cmd.ExtraFiles are mapped to descriptors starting at 3.
	ipcFile := os.NewFile(uintptr(3), "hotswap-child-ipc")
	return ipcFile, true
}

// startHotSwapSignalListener traps SIGUSR2 on Unix systems and triggers the handoff.
func startHotSwapSignalListener(ctx context.Context, cancel context.CancelFunc, opts docopt.Opts) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR2)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigChan:
				if sig == syscall.SIGUSR2 {
					_ = runHotSwapParentHandoff(ctx, cancel, opts)
				}
			}
		}
	}()
}

// sdNotify sends one datagram to systemd's NOTIFY_SOCKET. It is the single
// dial/write path shared by every sd_notify caller below; the protocol is
// newline-separated KEY=VALUE assignments in one message.
//
// Returns ErrNoNotifySocket when NOTIFY_SOCKET is unset, so callers can
// distinguish "not running under Type=notify" from a genuine send failure.
// notifySystemdMainPID needs that distinction (F-2); the fire-and-forget
// callers below deliberately swallow it.
func sdNotify(msg string) error {
	notifySocket := os.Getenv("NOTIFY_SOCKET")
	if notifySocket == "" {
		return ErrNoNotifySocket
	}

	addr := &net.UnixAddr{
		Name: notifySocket,
		Net:  "unixgram",
	}

	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return fmt.Errorf("dial NOTIFY_SOCKET %s: %w", notifySocket, err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(msg))
	return err
}

// notifySystemdMainPID sends MAINPID=<pid> to systemd's NOTIFY_SOCKET if present.
func notifySystemdMainPID(pid int) error {
	return sdNotify("MAINPID=" + strconv.Itoa(pid) + "\n")
}

// notifySystemdReady sends READY=1, meaning "startup is complete".
//
// READY is systemd's startup barrier, not a health signal: it answers "has
// this unit finished starting", not "is this unit currently working". The
// provider therefore sends it once it is self-managing (control socket bound,
// supervision loops running), NOT once a proxy has authenticated.
//
// Gating READY on a successful proxy auth would make the barrier hostage to a
// third party. The unit sets TimeoutStartSec=0, which is itself correct (proxy
// auth backs off for hours against a rate-limited API, so any finite timeout
// would eventually kill a provider that is retrying correctly). Combining the
// two means a `systemctl restart` during an API outage blocks forever, with no
// ceiling, at exactly the moment an operator most needs to restart. Health is
// reported separately and continuously via notifySystemdStatus.
func notifySystemdReady() error {
	if err := sdNotify("READY=1\n"); err != nil && !errors.Is(err, ErrNoNotifySocket) {
		return err
	}
	return nil
}

// notifySystemdStatus sends STATUS=<text>, the free-form line systemd shows in
// `systemctl status`. This is where proxy health belongs: it can be sent as
// often as state changes, before or after READY, and is purely informational,
// so it can never wedge the unit the way a withheld READY does.
func notifySystemdStatus(text string) error {
	if err := sdNotify("STATUS=" + text + "\n"); err != nil && !errors.Is(err, ErrNoNotifySocket) {
		return err
	}
	return nil
}

// execInPlace replaces the current process image in-place via syscall.Exec (execve)
// with an automatic retry loop if the Linux kernel returns ETXTBSY (F-17).
func execInPlace(exe string, args []string, env []string) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = syscall.Exec(exe, args, env)
		if errors.Is(err, syscall.ETXTBSY) {
			backoff := time.Duration(attempt*150) * time.Millisecond
			tlog("⚠️ [hotswap-heal] syscall.Exec hit ETXTBSY (attempt %d/3); retrying after %v...\n", attempt, backoff)
			time.Sleep(backoff)
			continue
		}
		break
	}
	return err
}
