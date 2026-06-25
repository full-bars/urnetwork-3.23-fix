//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	shmLogPath      = "/dev/shm/urnetwork.log"
	shmLogMaxSize   = 5 * 1024 * 1024 // 5MB target cap
	shmLogTrimRatio = 3               // keep newest 1/trimRatio, discard oldest (trimRatio-1)/trimRatio

	// Second, smaller buffer holding ONLY high-value lines (see
	// isImportantLogLine) so the earnings/health signal survives for hours
	// even when the main 5MB buffer floods in ~84s. Same /dev/shm (RAM) medium
	// as the main log, surfaced via `urnet-tools logs --important`.
	shmImportantLogPath    = "/dev/shm/urnetwork-important.log"
	shmImportantLogMaxSize = 1 * 1024 * 1024 // 1MB target cap
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

	// Second buffer: high-value lines only. Best-effort; nil disables mirroring.
	fImp, impErr := os.OpenFile(shmImportantLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if impErr != nil {
		fmt.Fprintf(os.Stderr, "failed to open important shm log: %v\n", impErr)
		fImp = nil
	}
	var fImpMu sync.Mutex

	go func() {
		defer fMu.Lock()
		defer f.Close()
		defer fMu.Unlock()
		defer r.Close()
		defer w.Close()

		buf := make([]byte, 32*1024)
		var lineBuf []byte // accumulates a partial trailing line across reads

		for {
			n, err := r.Read(buf)
			if n > 0 {
				fMu.Lock()
				f.Write(buf[:n])
				fMu.Unlock()

				// Mirror complete important lines into the small buffer. Both
				// tlog and glog output flow through this pipe, so this captures
				// [profit]/[earn]/[health]/etc. regardless of which logger emit.
				if fImp != nil {
					lineBuf = append(lineBuf, buf[:n]...)
					for {
						i := bytes.IndexByte(lineBuf, '\n')
						if i < 0 {
							break
						}
						line := lineBuf[:i+1] // include newline
						if isImportantLogLine(string(line)) {
							fImpMu.Lock()
							fImp.Write(line)
							fImpMu.Unlock()
						}
						lineBuf = lineBuf[i+1:]
					}
					if len(lineBuf) > 1<<20 { // guard a pathological no-newline stream
						lineBuf = lineBuf[:0]
					}
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// Trim the important buffer the same way as the main log.
	if fImp != nil {
		go func() {
			for {
				time.Sleep(5 * time.Second)
				fImpMu.Lock()
				fi, err := fImp.Stat()
				if err == nil && fi.Size() > shmImportantLogMaxSize {
					keep := fi.Size() * (shmLogTrimRatio - 1) / shmLogTrimRatio
					b := make([]byte, keep)
					fImp.ReadAt(b, fi.Size()-keep)
					fImp.Truncate(0)
					fImp.Seek(0, 0)
					fImp.Write(b)
				}
				fImp.Sync()
				fImpMu.Unlock()
			}
		}()
	}

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
