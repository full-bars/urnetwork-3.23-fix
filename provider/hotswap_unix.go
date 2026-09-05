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

// spawnHotSwapCandidate creates an anonymous socketpair, passes descriptor 3 to the child via ExtraFiles,
// and starts the candidate process.
func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "hotswap-parent")
	childFile := os.NewFile(uintptr(fds[1]), "hotswap-child")
	defer childFile.Close()

	cmd := exec.Command(exe, args...)
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
				cmd = exec.Command(exe, args...)
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
func getHotSwapChildIPC() (*os.File, bool) {
	if os.Getenv(EnvHotSwap) != "1" {
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
		return nil // Not running under systemd with Type=notify / NotifyAccess=all
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

// execInPlace replaces the current process image in-place via syscall.Exec (execve)
// with an automatic retry loop if the Linux kernel returns ETXTBSY.
func execInPlace(exe string, args []string, env []string) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = syscall.Exec(exe, args, env)
		if err == syscall.ETXTBSY {
			tlog("⚠️ [hotswap-heal] syscall.Exec hit ETXTBSY (attempt %d/3); retrying after 150ms...\n", attempt)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		break
	}
	return err
}

