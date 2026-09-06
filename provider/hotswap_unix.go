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
		_ = s.childCmd.Wait()
	}
	s.Close()
}

// Wait waits for the candidate child process to exit.
func (s *HotswapParentSession) Wait() error {
	if s.childCmd != nil {
		return s.childCmd.Wait()
	}
	return nil
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

// spawnHotSwapCandidate creates an anonymous socketpair with SOCK_CLOEXEC,
// passes descriptor 3 to the child via ExtraFiles, and starts the candidate process.
func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	// SOCK_CLOEXEC prevents descriptor leakage into child or unrelated forks (F-7)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
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

// notifySystemdMainPID sends MAINPID=<pid> to systemd's NOTIFY_SOCKET if present.
func notifySystemdMainPID(pid int) error {
	notifySocket := os.Getenv("NOTIFY_SOCKET")
	if notifySocket == "" {
		return ErrNoNotifySocket // Distinguishable error when NOTIFY_SOCKET is missing (F-2)
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

	msg := []byte("MAINPID=" + strconv.Itoa(pid) + "\n")
	_, err = conn.Write(msg)
	return err
}

// notifySystemdReady sends READY=1 to systemd's NOTIFY_SOCKET when the provider is active.
func notifySystemdReady() error {
	notifySocket := os.Getenv("NOTIFY_SOCKET")
	if notifySocket == "" {
		return nil
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

	_, err = conn.Write([]byte("READY=1\n"))
	return err
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
