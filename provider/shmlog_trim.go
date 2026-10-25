//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"time"
)

// mainTrimWarned and impTrimWarned track failure state so warnings repeat at
// a bounded rate rather than being silenced permanently.
var (
	mainTrimWarned   bool
	mainTrimWarnTime time.Time
	impTrimWarned    bool
	impTrimWarnTime  time.Time
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
	if maxKeep := maxSize * (ratio - 1) / ratio; keep > maxKeep {
		keep = maxKeep
	}
	if keep <= 0 {
		return nil
	}

	b := make([]byte, keep)
	if _, err := f.ReadAt(b, fi.Size()-keep); err != nil {
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
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	// f.Seek after Truncate restores the position to 0, which is defense-in-
	// depth against a future regression that drops O_APPEND. With O_APPEND
	// every Write already goes to end-of-file, so the Seek is redundant in
	// normal operation but prevents a stray positioned write from clobbering
	// the kept tail.
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write kept %d bytes: %w", len(b), err)
	}
	return nil
}

// reportTrimFailure writes a warning to the process's stderr, which is the
// RAM log itself once the redirect is in place. Unlike the original sync.Once
// approach, it re-emits after a 5-minute cooldown so a long-running daemon
// does not permanently silence the warning.
func reportTrimFailure(warned *bool, warnTime *time.Time, path string, err error) {
	now := time.Now()
	if *warned && now.Sub(*warnTime) < 5*time.Minute {
		return
	}
	*warned = true
	*warnTime = now
	fmt.Fprintf(os.Stderr, "[ramlogs] warning: cannot trim %s, leaving it oversized rather than losing history: %v\n", path, err)
}

// trimRAMLogByRename trims the log at path without anyone holding the writer's
// lock for the copy.
//
// trimRAMLog rewrites the file in place, which is correct but expensive: the
// caller has to hold the write lock across a read and a write of most of a
// multi-megabyte file, and while it does, the goroutine draining the 64 KiB
// pipe cannot write. The pipe fills, and then every glog emit blocks inside the
// stderr sink's mutex - a lock convoy that shows up as most of a core on a
// small box, and as dials that appear to stall.
//
// This variant builds the kept tail in a sibling file and renames it over the
// log. The rename is atomic within a directory, so a reader that opens the path
// sees either the old file or the trimmed one, and the caller only has to swap
// its own descriptor afterwards. The copy never runs under the writer's lock.
//
// It reports whether it trimmed; an unremarkable file is not an error.
func trimRAMLogByRename(path string, maxSize int64, ratio int64) (bool, error) {
	if ratio < 2 {
		return false, fmt.Errorf("trim ratio %d must be at least 2", ratio)
	}
	source, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open for trim: %w", err)
	}
	defer source.Close()

	fi, err := source.Stat()
	if err != nil {
		return false, fmt.Errorf("stat: %w", err)
	}
	if fi.Size() <= maxSize {
		return false, nil
	}

	keep := fi.Size() * (ratio - 1) / ratio
	if maxKeep := maxSize * (ratio - 1) / ratio; keep > maxKeep {
		keep = maxKeep
	}
	if keep <= 0 {
		return false, nil
	}

	b := make([]byte, keep)
	if _, err := source.ReadAt(b, fi.Size()-keep); err != nil {
		// Nothing has been written: the oversized log survives intact.
		return false, fmt.Errorf("read newest %d bytes: %w", keep, err)
	}
	// The cut lands mid-line, so drop the partial first line for the same
	// reason trimRAMLog does: line-oriented tooling must not open on half a line.
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}

	tmp := path + ".trim"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return false, fmt.Errorf("open trim target: %w", err)
	}
	if _, err := out.Write(b); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, fmt.Errorf("write kept %d bytes: %w", len(b), err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("close trim target: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("rename trim target: %w", err)
	}
	return true, nil
}
