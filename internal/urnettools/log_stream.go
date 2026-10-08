package urnettools

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// streamLogFile reads the last n lines of a file, writes them to out, and follows
// the file for newly appended content until ctx is cancelled or os.Interrupt is received.
// Designed to replace external 'tail -f' invocations cross-platform.
func streamLogFile(ctx context.Context, path string, n int, out io.Writer) error {
	f, err := openFollowFile(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	printTailLines(f, n, out)

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
	}

	buf := make([]byte, 4096)
	// drain copies everything currently readable from the open file to out.
	drain := func() error {
		for {
			nr, err := f.Read(buf)
			if nr > 0 {
				offset += int64(nr)
				if _, wErr := out.Write(buf[:nr]); wErr != nil {
					return wErr
				}
			}
			if err != nil {
				return nil
			}
		}
	}

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			if fi.Size() < offset {
				// File truncated: seek back to 0.
				offset = 0
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return err
				}
			}
			if err := drain(); err != nil {
				return err
			}
			// The path may now name a different file (rotation: the log was
			// renamed away and a new one created). Finish the old file above,
			// then follow the new one from its start. If the path is gone or
			// cannot be opened yet, keep the old handle and look again next tick.
			if pathInfo, statErr := os.Stat(path); statErr == nil && !os.SameFile(fi, pathInfo) {
				next, openErr := openFollowFile(path)
				if openErr != nil {
					continue
				}
				f.Close()
				f = next
				offset = 0
				if err := drain(); err != nil {
					return err
				}
			}
		}
	}
}

// printTailLines reads up to the last n lines from f and writes them to out.
func printTailLines(f *os.File, n int, out io.Writer) {
	if n <= 0 {
		return
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return
	}

	const maxTailBytes = 64 * 1024
	readSize := fi.Size()
	var startOffset int64
	if readSize > maxTailBytes {
		readSize = maxTailBytes
		startOffset = fi.Size() - maxTailBytes
	}

	buf := make([]byte, readSize)
	nr, err := f.ReadAt(buf, startOffset)
	if err != nil && err != io.EOF {
		return
	}
	buf = buf[:nr]

	lines := strings.Split(string(buf), "\n")
	hasTrailingNewline := len(lines) > 0 && lines[len(lines)-1] == ""
	if hasTrailingNewline {
		lines = lines[:len(lines)-1]
	}

	if startOffset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
}
