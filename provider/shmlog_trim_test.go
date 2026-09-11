//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTestLog opens a temp file the way the RAM logger does, so the tests
// exercise the real descriptor flags rather than a convenient read-write one.
func openTestLog(t *testing.T, flags int) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "urnetwork.log")
	f, err := os.OpenFile(path, os.O_CREATE|flags|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f, path
}

// writeLines fills the file with numbered lines until it exceeds size.
func writeLines(t *testing.T, f *os.File, size int64) {
	t.Helper()
	for i := 0; ; i++ {
		if _, err := fmt.Fprintf(f, "line %06d padding padding padding padding padding\n", i); err != nil {
			t.Fatalf("write: %v", err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Size() > size {
			return
		}
	}
}

// TestTrimRAMLogKeepsNewestContent is the regression test for the defect that
// shipped: the trim wrote back a zero-filled buffer, so the file kept its size
// in RAM and lost every byte of content. Assert on the CONTENT, not the size —
// a size check alone passes against a file full of NULs.
func TestTrimRAMLogKeepsNewestContent(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	const maxSize = 64 * 1024
	writeLines(t, f, maxSize)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	lastLine := lastCompleteLine(before)

	if err := trimRAMLog(f, maxSize, shmLogTrimRatio); err != nil {
		t.Fatalf("trim: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}

	if bytes.IndexByte(after, 0) >= 0 {
		t.Fatalf("trimmed file contains NUL bytes: the kept region was not read back (first NUL at %d of %d)",
			bytes.IndexByte(after, 0), len(after))
	}
	if len(after) >= len(before) {
		t.Errorf("file did not shrink: %d -> %d", len(before), len(after))
	}
	if !bytes.Contains(after, []byte(lastLine)) {
		t.Errorf("newest line %q missing after trim", lastLine)
	}
	if bytes.Contains(after, []byte("line 000000 ")) {
		t.Error("oldest line survived the trim")
	}
	if !bytes.HasPrefix(after, []byte("line ")) {
		t.Errorf("file starts mid-line: %q", string(after[:min(40, len(after))]))
	}
}

// TestTrimRAMLogFailsClosedOnWriteOnlyFD pins the fail-closed contract against
// the exact descriptor that caused the incident. A write-only fd cannot be read
// back, so the trim must report the error and leave the file untouched rather
// than replacing its contents with padding.
func TestTrimRAMLogFailsClosedOnWriteOnlyFD(t *testing.T) {
	f, path := openTestLog(t, os.O_WRONLY)
	const maxSize = 32 * 1024
	writeLines(t, f, maxSize)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	err = trimRAMLog(f, maxSize, shmLogTrimRatio)
	if err == nil {
		t.Fatal("trim on a write-only descriptor returned nil error")
	}
	if !strings.Contains(err.Error(), "read newest") {
		t.Errorf("error does not name the failing step: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("file was modified despite the failed read: %d bytes -> %d bytes", len(before), len(after))
	}
	if bytes.IndexByte(after, 0) >= 0 {
		t.Error("file contains NUL bytes: the failed trim wrote padding")
	}
}

// TestTrimRAMLogUnderCapIsNoOp asserts the common path does nothing at all.
// The trimmer runs every 5 seconds for the life of the process.
func TestTrimRAMLogUnderCapIsNoOp(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	if _, err := f.WriteString("short\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := trimRAMLog(f, 64*1024, shmLogTrimRatio); err != nil {
		t.Fatalf("trim: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "short\n" {
		t.Errorf("under-cap file changed: %q", string(b))
	}
}

// TestTrimRAMLogAppendsAfterTrim covers the sequence that actually happens in
// production: the writer keeps appending through the same descriptor after a
// trim. O_APPEND means the offset is irrelevant, but a regression that dropped
// O_APPEND or reintroduced a Seek would strand writes past a NUL hole.
func TestTrimRAMLogAppendsAfterTrim(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	const maxSize = 32 * 1024
	writeLines(t, f, maxSize)

	if err := trimRAMLog(f, maxSize, shmLogTrimRatio); err != nil {
		t.Fatalf("trim: %v", err)
	}
	if _, err := f.WriteString("after the trim\n"); err != nil {
		t.Fatalf("write after trim: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		t.Fatalf("hole in file after post-trim append (first NUL at %d of %d)", bytes.IndexByte(b, 0), len(b))
	}
	if !bytes.HasSuffix(b, []byte("after the trim\n")) {
		t.Error("post-trim write did not land at end of file")
	}
}

// TestTrimRAMLogRepeatedTrimsStayBounded runs the loop the goroutine runs, so a
// drift that grows the file across cycles cannot pass.
func TestTrimRAMLogRepeatedTrimsStayBounded(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	const maxSize = 16 * 1024

	for cycle := 0; cycle < 6; cycle++ {
		writeLines(t, f, maxSize)
		if err := trimRAMLog(f, maxSize, shmLogTrimRatio); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Size() > maxSize {
			t.Fatalf("cycle %d: size %d exceeds cap %d after trim", cycle, fi.Size(), maxSize)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		t.Error("NUL bytes accumulated across trim cycles")
	}
}

// TestTrimRAMLogRejectsBadRatio guards the arithmetic: ratio 1 would compute a
// zero-byte keep and ratio 0 would divide by zero.
func TestTrimRAMLogRejectsBadRatio(t *testing.T) {
	f, _ := openTestLog(t, os.O_RDWR)
	writeLines(t, f, 8*1024)
	for _, ratio := range []int64{0, 1, -3} {
		if err := trimRAMLog(f, 8*1024, ratio); err == nil {
			t.Errorf("ratio %d accepted", ratio)
		}
	}
}

func TestTrimRAMLogNilFile(t *testing.T) {
	if err := trimRAMLog(nil, 1024, shmLogTrimRatio); err != nil {
		t.Errorf("nil file: %v", err)
	}
}

// TestReportTrimFailureWarnsOnce asserts the warning does not become the log
// spam it is reporting on: the trimmer retries every 5 seconds forever.
func TestReportTrimFailureWarnsOnce(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	realStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = realStderr })

	var once sync.Once
	for i := 0; i < 5; i++ {
		reportTrimFailure(&once, "/dev/shm/test.log", fmt.Errorf("bad file descriptor"))
	}
	w.Close()

	out, err := os.ReadFile("/proc/self/fd/" + fmt.Sprint(int(r.Fd())))
	if err != nil {
		buf := new(bytes.Buffer)
		if _, cerr := buf.ReadFrom(r); cerr != nil {
			t.Fatalf("read pipe: %v", cerr)
		}
		out = buf.Bytes()
	}
	if n := bytes.Count(out, []byte("cannot trim")); n != 1 {
		t.Errorf("warned %d times, want 1: %q", n, string(out))
	}
}

func lastCompleteLine(b []byte) string {
	trimmed := bytes.TrimRight(b, "\n")
	if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
		return string(trimmed[i+1:])
	}
	return string(trimmed)
}
