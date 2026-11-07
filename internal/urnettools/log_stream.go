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
// then resumes after matching previously printed content, or from its start if
// there is no match. A truncated file is followed from its start. If the path
// is briefly missing it keeps the old file and looks again.
func streamLogFile(ctx context.Context, path string, n int, out io.Writer) error {
	f, err := openFollowFile(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	history := &logHistory{out: out}
	out = history
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
				offset = resumeLogOffset(next, history.data)
				if _, err := next.Seek(offset, io.SeekStart); err != nil {
					next.Close()
					return err
				}
				f.Close()
				f = next
				if err := drain(); err != nil {
					return err
				}
			}
		}
	}
}

const logHistoryLimit = 256 * 1024

type logHistory struct {
	out  io.Writer
	data []byte
}

func (h *logHistory) Write(p []byte) (int, error) {
	n, err := h.out.Write(p)
	p = p[:n]
	if len(p) >= logHistoryLimit {
		h.data = append(h.data[:0], p[len(p)-logHistoryLimit:]...)
	} else {
		if excess := len(h.data) + len(p) - logHistoryLimit; excess > 0 {
			copy(h.data, h.data[excess:])
			h.data = h.data[:len(h.data)-excess]
		}
		h.data = append(h.data, p...)
	}
	return n, err
}

// resumeLogOffsetCandidates bounds how many line-ended windows are tried. The
// step-back only has to cover the few lines the provider writes to the old inode
// between copying the kept tail and swapping its descriptor, which is a handful at
// most. Each miss scans the whole new file, so a smaller bound keeps an unrelated
// replacement cheap.
const resumeLogOffsetCandidates = 16

// resumeLogOffset finds where in the new file the printed history ends, so the
// follower resumes there instead of replaying the file. It searches line-ended
// windows of the history from the end, stepping back over writes that reached the
// old inode after the replacement was copied. 0 means no overlap: follow the new
// file from its start.
//
// When the old file ended mid-line, the fragment was already printed and the new
// file holds the whole line, so a match on the last window also skips that
// fragment rather than printing it twice.
func resumeLogOffset(f *os.File, history []byte) int64 {
	const maxFile = 32 * 1024 * 1024
	if len(history) == 0 {
		return 0
	}
	content, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(content) > maxFile {
		return 0
	}
	end := bytes.LastIndexByte(history, '\n') + 1
	fragment := history[end:]
	for candidates := 0; end > 0 && candidates < resumeLogOffsetCandidates; candidates++ {
		start := end - 1024
		if start < 0 {
			start = 0
		}
		if pos := longestHistoryOverlap(content, history[:end], end-start); pos >= 0 {
			if candidates == 0 && len(fragment) > 0 && bytes.HasPrefix(content[pos:], fragment) {
				pos += len(fragment)
			}
			return int64(pos)
		}
		end = bytes.LastIndexByte(history[:end-1], '\n') + 1
	}
	return 0
}

// longestHistoryOverlap selects the strongest suffix match, preferring the
// earliest end when matches are indistinguishable so new output is not skipped.
func longestHistoryOverlap(content, history []byte, minimum int) int {
	pattern := make([]byte, len(history))
	for i := range history {
		pattern[i] = history[len(history)-1-i]
	}
	prefix := make([]int, len(pattern))
	for i, j := 1, 0; i < len(pattern); i++ {
		for j > 0 && pattern[i] != pattern[j] {
			j = prefix[j-1]
		}
		if pattern[i] == pattern[j] {
			j++
		}
		prefix[i] = j
	}
	bestLength, bestEnd, matched := 0, -1, 0
	for i := len(content) - 1; i >= 0; i-- {
		for matched > 0 && content[i] != pattern[matched] {
			matched = prefix[matched-1]
		}
		if content[i] == pattern[matched] {
			matched++
		}
		if matched >= minimum && (matched > bestLength || matched == bestLength && i+matched < bestEnd) {
			bestLength, bestEnd = matched, i+matched
		}
		if matched == len(pattern) {
			matched = prefix[matched-1]
		}
	}
	return bestEnd
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
