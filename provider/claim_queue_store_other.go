//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

// A platform without the required queue custody primitives cannot start a
// claim daemon. Read-only config loading remains available.
package main

import (
	"errors"
	"os"
)

// There is no unlocked fallback for claim queue ownership.
func claimQueuePlatformSupported() error {
	return errors.New("claim queue directory ownership is unsupported on this platform")
}

// Unsupported platforms never acquire a queue namespace.
func claimQueueOpenDirectory(string) (*os.File, error) {
	return nil, claimQueuePlatformSupported()
}

// Unsupported platforms cannot authenticate an owned directory.
func claimQueuePrivateDirectory(*os.File) error { return claimQueuePlatformSupported() }

func claimQueueLockDirectory(*os.File) error { return claimQueuePlatformSupported() }

// Unsupported platforms never consume retained queue custody.
func claimQueueReadFile(*os.File, string, claimQueueReadHooks) ([]byte, os.FileMode, error) {
	return nil, 0, claimQueuePlatformSupported()
}

// Unsupported platforms never publish queue state.
func claimQueuePublish(*os.File, string, []byte) error { return claimQueuePlatformSupported() }
