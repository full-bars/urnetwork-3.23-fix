package urnettools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is an io.Writer the follower writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// follower runs streamLogFile in the background. Every wait polls a condition
// under a generous deadline, so a test's outcome never depends on how fast the
// runner is.
type follower struct {
	t    *testing.T
	out  *syncBuffer
	stop func()
}

func startFollower(t *testing.T, path string, n int) *follower {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{t: t, out: &syncBuffer{}}
	done := make(chan error, 1)
	go func() { done <- streamLogFile(ctx, path, n, f.out) }()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("streamLogFile: %v", err)
			}
		})
	}
	t.Cleanup(f.stop)
	return f
}

// waitFor blocks until the follower has written want.
func (f *follower) waitFor(want string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(f.out.String(), want) {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %q; output so far %q", want, f.out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamLogFile_PrintTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\nline 2\nline 3\nline 4\nline 5\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled up front: streamLogFile prints the tail and returns

	var out bytes.Buffer
	if err := streamLogFile(ctx, logPath, 2, &out); err != nil {
		t.Fatalf("streamLogFile: %v", err)
	}
	if got, want := out.String(), "line 4\nline 5\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The tail keeps an unfinished last line as it is, so the rest of that line,
// appended later, continues it instead of starting a new one.
func TestStreamLogFile_UnfinishedLastLineContinues(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\nhalf of a li"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 2)
	f.waitFor("half of a li")
	appendLine(t, logPath, "ne\n")
	f.waitFor("half of a line\n")
	if got, want := f.out.String(), "line 1\nhalf of a line\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamLogFile_FollowAppended(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 1)
	f.waitFor("line 1")
	appendLine(t, logPath, "line 2\n")
	f.waitFor("line 2")
	if got, want := f.out.String(), "line 1\nline 2\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Every line is written exactly once however many polls pass. This also catches
// a follower that wrongly decides the file was replaced and re-reads it.
func TestStreamLogFile_EachLineExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 3)
	f.waitFor("c\n")
	for _, line := range []string{"d", "e", "f", "g"} {
		appendLine(t, logPath, line+"\n")
		f.waitFor(line + "\n")
	}
	if got, want := f.out.String(), "a\nb\nc\nd\ne\nf\ng\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The tail returns the offset it read up to, not the end of the file, so the
// follower can start there and lose nothing appended in between.
func TestPrintTailLines_ReturnsWhereItStoppedReading(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	end := printTailLines(f, 2, &out)
	appendLine(t, logPath, "three\n") // lands after the tail was read
	if got, want := out.String(), "one\ntwo\n"; got != want {
		t.Fatalf("tail got %q, want %q", got, want)
	}
	if want := int64(len("one\ntwo\n")); end != want {
		t.Fatalf("printTailLines returned offset %d, want %d", end, want)
	}
}

// A rename of the followed log must not be blocked by the follower. On Windows
// this fails with ERROR_SHARING_VIOLATION unless the file was opened with
// FILE_SHARE_DELETE; on other systems it passes without the fix too, so it only
// pins the Windows open.
func TestStreamLogFile_DoesNotBlockRenameOrDelete(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 1)
	f.waitFor("line 1") // the follower has the file open now
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatalf("rename of a followed log failed: %v", err)
	}
	if err := os.Remove(logPath + ".1"); err != nil {
		t.Fatalf("delete of a followed log failed: %v", err)
	}
}

// After the log is renamed away and a new file is created at the same path
// (rotation), the follower finishes the old file and then follows the new one.
func TestStreamLogFile_FollowsRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 1)
	f.waitFor("line 1")

	appendLine(t, logPath, "last of old\n")
	f.waitFor("last of old") // read from the old file before it is rotated away
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	appendLine(t, logPath, "first of new\n")
	f.waitFor("first of new")
	appendLine(t, logPath, "second of new\n")
	f.waitFor("second of new")

	if got, want := f.out.String(), "line 1\nlast of old\nfirst of new\nsecond of new\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Lines written to the old file after the last poll but before the follower
// notices the rotation are still delivered, before the new file's first line.
func TestStreamLogFile_DrainsOldFileBeforeSwitching(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 1)
	f.waitFor("line 1")

	// Rotate and write to both files back to back; the follower may see all of
	// it in one poll. The old file's line must still come first.
	appendLine(t, logPath, "tail of old\n")
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	appendLine(t, logPath, "head of new\n")
	f.waitFor("head of new")

	got := f.out.String()
	if i, j := strings.Index(got, "tail of old"), strings.Index(got, "head of new"); i < 0 || j < 0 || i > j {
		t.Fatalf("old file's last line must come before the new file's first: %q", got)
	}
}

// A truncated log is followed from its start again.
func TestStreamLogFile_FollowsTruncate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("a long first line of text\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := startFollower(t, logPath, 1)
	f.waitFor("a long first line of text")
	if err := os.Truncate(logPath, 0); err != nil {
		t.Fatal(err)
	}
	appendLine(t, logPath, "after\n")
	f.waitFor("after\n")

	if got, want := f.out.String(), "a long first line of text\nafter\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A line appended after the tail was printed and before following starts must
// still be delivered, exactly once: following starts where the tail stopped
// reading, not at the end of the file.
func TestStreamLogFile_LineAppendedBetweenTailAndFollowIsDelivered(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	orig := streamLogFileAfterTail
	streamLogFileAfterTail = func() { appendLine(t, logPath, "three\n") }
	t.Cleanup(func() { streamLogFileAfterTail = orig })

	f := startFollower(t, logPath, 2)
	f.waitFor("three\n")
	if got, want := f.out.String(), "one\ntwo\nthree\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The pre-switch drain is what delivers a line written to the old file after the
// follower last read it but before it opens the new one. The hook writes that
// line inside exactly that window, so removing the drain loses it.
func TestStreamLogFile_PreSwitchDrainDeliversLateOldLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	streamLogFileBeforeSwitch = func() {
		once.Do(func() { appendLine(t, logPath+".1", "late old\n") })
	}
	t.Cleanup(func() { streamLogFileBeforeSwitch = func() {} })

	f := startFollower(t, logPath, 1)
	f.waitFor("line 1")
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	appendLine(t, logPath, "head of new\n")
	f.waitFor("head of new")

	if got, want := f.out.String(), "line 1\nlate old\nhead of new\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// If the path stat claims a different file while the path still names the same
// one, the follower must not restart from the top and print the log twice.
func TestStreamLogFile_SpuriousDifferentStatDoesNotReprint(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	otherPath := filepath.Join(dir, "other.log")
	if err := os.WriteFile(logPath, []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	statFollowPath = func(string) (os.FileInfo, error) { return os.Stat(otherPath) }
	t.Cleanup(func() { statFollowPath = os.Stat })

	f := startFollower(t, logPath, 1)
	f.waitFor("one")
	// let several polls pass with the spurious stat in effect
	time.Sleep(600 * time.Millisecond)
	appendLine(t, logPath, "two\n")
	f.waitFor("two")

	if got, want := f.out.String(), "one\ntwo\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
