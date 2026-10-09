package urnettools

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// streamLogFileAfterTail runs between printing the tail and starting to follow.
// It is a no-op outside tests, which use it to append a line in that gap.
var streamLogFileAfterTail = func() {}

// streamLogFileBeforeSwitch runs once the follower has decided the path names a
// different file, before it opens that file. No-op outside tests, which use it
// to write to the old file in that window.
var streamLogFileBeforeSwitch = func() {}

// statFollowPath stats the followed path. A variable so a test can make the
// path look like a different file, which on Windows happens when the file id of
// an unchanged file cannot be loaded for a moment.
var statFollowPath = os.Stat

// streamLogFile reads the last n lines of a file, writes them to out, and follows
// the file for newly appended content until ctx is cancelled or os.Interrupt is received.
// Designed to replace external 'tail -f' invocations cross-platform.
//
// It follows the PATH, not just the open file: if the path starts naming a
// different file (the log was rotated or replaced) it finishes the old file and
// then follows the new one from its start. A truncated file is followed from its
// start. If the path is briefly missing it keeps the old file and looks again.
func streamLogFile(ctx context.Context, path string, n int, out io.Writer) error {
	f, err := openFollowFile(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	// follow from exactly where the tail stopped reading, so a line appended
	// while the tail was being written is not skipped
	offset := printTailLines(f, n, out)
	streamLogFileAfterTail()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
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
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
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
			// The path may now name a different file (rotation). If it does,
			// open the new one first, then read whatever the old one received
			// in the meantime, then switch. If the path is gone or cannot be
			// opened yet, keep the old handle and look again next tick.
			if pathInfo, statErr := statFollowPath(path); statErr == nil && !os.SameFile(fi, pathInfo) {
				streamLogFileBeforeSwitch()
				next, openErr := openFollowFile(path)
				if openErr != nil {
					continue
				}
				// os.SameFile on a path stat can say "different" for an unchanged
				// file (Windows loads the file id lazily and a failed load makes
				// it false). Compare the handles instead: switching onto the same
				// file would restart at offset 0 and print the whole log again.
				if nextInfo, nextErr := next.Stat(); nextErr != nil || os.SameFile(fi, nextInfo) {
					next.Close()
					continue
				}
				if err := drain(); err != nil {
					next.Close()
					return err
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

// printTailLines writes the last n lines of f to out exactly as they are in the
// file (an unfinished last line stays unfinished, so what is appended later
// continues it) and returns the offset it read up to. A non-positive n writes
// nothing and returns the current size.
func printTailLines(f *os.File, n int, out io.Writer) int64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	size := fi.Size()
	if n <= 0 || size == 0 {
		return size
	}

	const maxTailBytes = 64 * 1024
	readSize := size
	var startOffset int64
	if readSize > maxTailBytes {
		readSize = maxTailBytes
		startOffset = size - maxTailBytes
	}

	// For a window that starts mid-file, also read the byte just before it: if
	// that byte is a newline the window starts exactly on a line boundary and
	// its first line is complete, not a fragment to drop.
	readFrom := startOffset
	if startOffset > 0 {
		readFrom = startOffset - 1
	}
	buf := make([]byte, readSize+(startOffset-readFrom))
	nr, err := f.ReadAt(buf, readFrom)
	if err != nil && err != io.EOF {
		return readFrom + int64(nr)
	}
	buf = buf[:nr]
	end := readFrom + int64(nr)
	startsOnLine := false
	if startOffset > 0 {
		if len(buf) == 0 {
			return end
		}
		startsOnLine = buf[0] == '\n'
		buf = buf[1:]
	}

	// the bytes before the n-th newline counted from the end are not part of the tail
	body := buf
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	start := 0
	count := 0
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '\n' {
			count++
			if count == n {
				start = i + 1
				break
			}
		}
	}
	// a window that starts mid-file begins with a partial line: drop it
	if count < n && startOffset > 0 && !startsOnLine {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return end
		}
		start = i + 1
	}
	out.Write(buf[start:])
	return end
}
