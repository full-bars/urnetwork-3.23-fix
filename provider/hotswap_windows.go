//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"

	"github.com/docopt/docopt-go"
)

// HotswapParentSession tracks the spawned candidate process on Windows (stub).
type HotswapParentSession struct {
	Reader *bufio.Reader
	Writer io.Writer
}

func (s *HotswapParentSession) Close() {}
func (s *HotswapParentSession) Kill()  {}
func (s *HotswapParentSession) Wait() error {
	return nil
}

func spawnHotSwapCandidate(exe string, args []string) (*HotswapParentSession, error) {
	return nil, errors.New("hotswap not yet implemented on windows (requires named pipe adapter)")
}

func getHotSwapChildIPC() (*os.File, bool) {
	return nil, false
}

func startHotSwapSignalListener(ctx context.Context, cancel context.CancelFunc, opts docopt.Opts) {
	// No-op on Windows until named pipe adapter is connected
}

func notifySystemdMainPID(pid int) error {
	return nil
}

func execInPlace(exe string, args []string, env []string) error {
	return errors.New("execve not supported on Windows")
}

