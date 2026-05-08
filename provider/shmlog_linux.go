//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

const (
	shmLogPath    = "/dev/shm/urnetwork.log"
	shmLogMaxSize = 1024 * 1024 // 1MB
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
	syscall.Dup2(int(w.Fd()), int(os.Stdout.Fd()))
	syscall.Dup2(int(w.Fd()), int(os.Stderr.Fd()))

	go func() {
		defer f.Close()
		defer r.Close()
		defer w.Close()

		buf := make([]byte, 32*1024)
		var totalWritten int64

		for {
			n, err := r.Read(buf)
			if n > 0 {
				// If we exceed the max size, truncate and reset
				if totalWritten+int64(n) > shmLogMaxSize {
					f.Truncate(0)
					f.Seek(0, 0)
					totalWritten = 0
					f.Write([]byte("--- Log truncated due to size limit ---\n"))
				}
				
				wn, _ := f.Write(buf[:n])
				totalWritten += int64(wn)
				
				// Also write to a small internal buffer or just sync?
				// f.Sync() // Syncing to RAM disk is fast but we can do it periodically
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
			f.Sync()
		}
	}()
}
