//go:build !windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

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
