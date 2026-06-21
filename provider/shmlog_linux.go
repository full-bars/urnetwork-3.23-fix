//go:build linux

package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	shmLogPath      = "/dev/shm/urnetwork.log"
	shmLogMaxSize   = 5 * 1024 * 1024 // 5MB target cap
	shmLogTrimRatio = 3                // keep newest 1/trimRatio, discard oldest (trimRatio-1)/trimRatio
)

func initSHMLogger() {
	f, err := os.OpenFile(shmLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open shm log: %v\n", err)
		return
	}

	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pipe: %v\n", err)
		return
	}

	dup2(int(w.Fd()), int(os.Stdout.Fd()))
	dup2(int(w.Fd()), int(os.Stderr.Fd()))

	var fMu sync.Mutex

	go func() {
		defer fMu.Lock()
		defer f.Close()
		defer fMu.Unlock()
		defer r.Close()
		defer w.Close()

		buf := make([]byte, 32*1024)

		for {
			n, err := r.Read(buf)
			if n > 0 {
				fMu.Lock()
				f.Write(buf[:n])
				fMu.Unlock()
			}
			if err != nil {
				break
			}
		}
	}()

	// Trim the file down when it exceeds shmLogMaxSize, keeping the newest
	// portion and discarding the oldest. This is a ring-buffer-lite: the
	// file stays near 5MB, the most recent content is always preserved, and
	// the tail loop in urnet-tools logs (which reopens on fi.Size() < pos)
	// handles the size change transparently.
	go func() {
		for {
			time.Sleep(5 * time.Second)
			fMu.Lock()
			fi, err := f.Stat()
			if err == nil && fi.Size() > shmLogMaxSize {
				keep := fi.Size() * (shmLogTrimRatio - 1) / shmLogTrimRatio
				b := make([]byte, keep)
				f.ReadAt(b, fi.Size()-keep)
				f.Truncate(0)
				f.Seek(0, 0)
				f.Write(b)
			}
			fMu.Unlock()
		}
	}()

	// Periodically sync to ensure tail -f sees updates quickly
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			fMu.Lock()
			f.Sync()
			fMu.Unlock()
		}
	}()
}

func shmLogFatal(code int, format string, args ...any) {
	msg := fmt.Sprintf("FATAL [exit %d]: %s\n", code, fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(shmLogPath, os.O_WRONLY|os.O_APPEND, 0); err == nil {
		f.Write([]byte(msg))
		f.Sync()
		f.Close()
	}
	os.Stderr.Write([]byte(msg))
	os.Exit(code)
}
