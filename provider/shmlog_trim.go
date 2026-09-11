//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// mainTrimWarned and impTrimWarned keep a failed trim from reporting on every
// 5-second cycle. A trim failure is a persistent condition, not an event.
var (
	mainTrimWarned sync.Once
	impTrimWarned  sync.Once
)

// trimRAMLog bounds a RAM log file by discarding its oldest 1/ratio and
// keeping the newest (ratio-1)/ratio, in place, so the file descriptor and any
// `tail -f` following it stay valid.
//
// It fails closed. If the kept portion cannot be read back in full, the file is
// left exactly as it is: an oversized log that can still be read beats a
// correctly-sized log whose contents are gone. The original implementation did
// the opposite by accident. It read into a freshly allocated buffer, ignored
// the error, and wrote the buffer back regardless, so a failed read replaced
// the history with an equally long run of NUL bytes. Because the file lives on
// tmpfs, those NULs also went on occupying the RAM the cap exists to bound:
// the trim freed nothing and destroyed everything it claimed to preserve.
//
// f must be opened for reading as well as writing. That was the original
// defect: the log was opened O_WRONLY, so every ReadAt here returned EBADF.
func trimRAMLog(f *os.File, maxSize int64, ratio int64) error {
	if f == nil {
		return nil
	}
	if ratio < 2 {
		return fmt.Errorf("trim ratio %d must be at least 2", ratio)
	}

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if fi.Size() <= maxSize {
		return nil
	}

	keep := fi.Size() * (ratio - 1) / ratio
	if keep <= 0 {
		return nil
	}

	b := make([]byte, keep)
	if _, err := io.ReadFull(io.NewSectionReader(f, fi.Size()-keep, keep), b); err != nil {
		// Nothing is written: the oversized file survives intact.
		return fmt.Errorf("read newest %d bytes: %w", keep, err)
	}

	// The cut lands mid-line, so drop the partial first line. A log that opens
	// on half a line is confusing to read and breaks line-oriented tooling.
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	// No Seek before this write: f is O_APPEND, which forces every write to
	// end-of-file and ignores the offset entirely. After the truncate above,
	// end-of-file is 0, so this rewrites the file from the start.
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write kept %d bytes: %w", len(b), err)
	}
	return nil
}

// reportTrimFailure writes one warning per condition to the process's stderr,
// which is the RAM log itself once the redirect is in place.
func reportTrimFailure(once *sync.Once, path string, err error) {
	once.Do(func() {
		fmt.Fprintf(os.Stderr, "[ramlogs] warning: cannot trim %s, leaving it oversized rather than losing history: %v\n", path, err)
	})
}
