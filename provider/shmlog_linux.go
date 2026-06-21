//go:build linux

package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	shmLogPath        = "/dev/shm/urnetwork.log"
	shmLogRotatedPath = "/dev/shm/urnetwork.log.1"
	shmLogMaxSize     = 5 * 1024 * 1024 // 5MB
)

func initSHMLogger() {
	// Create or truncate the log file in RAM disk
	f, err := os.OpenFile(shmLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open shm log: %v\n", err)
		return
	}

	// Use a pipe to intercept stdout and stderr
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pipe: %v\n", err)
		return
	}

	// Redirect stdout and stderr to the write end of the pipe
	dup2(int(w.Fd()), int(os.Stdout.Fd()))
	dup2(int(w.Fd()), int(os.Stderr.Fd()))

	// fMu guards f: the writer goroutine reassigns it on rotation, and the
	// periodic-sync goroutine below reads it concurrently.
	var fMu sync.Mutex

	go func() {
		defer func() {
			fMu.Lock()
			f.Close()
			fMu.Unlock()
		}()
		defer r.Close()
		defer w.Close()

		buf := make([]byte, 32*1024)
		var totalWritten int64

		for {
			n, err := r.Read(buf)
			if n > 0 {
				// Once the active file hits the size cap, rotate it to .1
				// instead of truncating to zero. A pure truncate threw away
				// every line of history on every rotation, leaving only the
				// last ~75-90 minutes visible on a busy node — rotating
				// preserves a full extra cap's worth (up to ~5MB more) of
				// older history in the .1 file for incident investigation.
				if totalWritten+int64(n) > shmLogMaxSize {
					fMu.Lock()
					f.Close()
					os.Rename(shmLogPath, shmLogRotatedPath)
					newF, openErr := os.OpenFile(shmLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
					if openErr != nil {
						// Can't rotate (e.g. disk pressure) — fall back to
						// reusing the same handle and just resetting it, so
						// logging doesn't die outright.
						f.Truncate(0)
						f.Seek(0, 0)
					} else {
						f = newF
					}
					fMu.Unlock()
					totalWritten = 0
				}

				fMu.Lock()
				wn, _ := f.Write(buf[:n])
				fMu.Unlock()
				totalWritten += int64(wn)
			}
			if err != nil {
				break
			}
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

// shmLogFatal writes a message to both stderr and the ramlog file, then exits.
// Writing directly to the ramlog file bypasses the pipe goroutine so the
// message is guaranteed to be on disk even when os.Exit kills the process
// before the pipe reader can flush.
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
