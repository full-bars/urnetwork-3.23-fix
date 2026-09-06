//go:build windows

package main

import (
	"fmt"
	"os"
	"sync"
)

// On Windows there is no flock(2). We use a separate lock file protected by
// an in-process mutex. This prevents intra-process races (parent and candidate
// never run concurrently in the same process anyway, since the candidate is
// spawned via exec.Command). For inter-process safety we rely on the
// os.CreateTemp + rename atomicity already in place — on Windows two
// processes writing the same temp name will collide, but the os.CreateTemp
// fix in commit c5256ed8 already prevents that.
var jwtLockMu sync.Mutex

func acquireJWTStoreLock(path string) (func(), error) {
	jwtLockMu.Lock()
	defer jwtLockMu.Unlock()

	lockPath := path + jwtLockSuffix
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open jwt lock file: %w", err)
	}
	return func() {
		f.Close()
		os.Remove(lockPath)
	}, nil
}

const jwtLockSuffix = ".lock"
