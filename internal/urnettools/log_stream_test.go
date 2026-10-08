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

func TestStreamLogFile_PrintTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	content := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so streamLogFile returns after printing tail

	var out bytes.Buffer
	if err := streamLogFile(ctx, logPath, 2, &out); err != nil {
		t.Fatalf("streamLogFile: %v", err)
	}

	got := strings.TrimSpace(out.String())
	want := "line 4\nline 5"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStreamLogFile_FollowAppended(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- streamLogFile(ctx, logPath, 1, &out)
	}()

	// Wait for streamLogFile to print initial tail and seek to end
	time.Sleep(100 * time.Millisecond)

	// Append a line
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line 2\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Wait for ticker poll
	time.Sleep(300 * time.Millisecond)
	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("streamLogFile: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "line 1") || !strings.Contains(got, "line 2") {
		t.Fatalf("expected both lines, got %q", got)
	}
}

// followFor runs streamLogFile in the background and returns a function that
// stops it and returns everything it wrote.
func followFor(t *testing.T, path string, n int) (stop func() string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	errCh := make(chan error, 1)
	go func() { errCh <- streamLogFile(ctx, path, n, &out) }()
	time.Sleep(100 * time.Millisecond) // let it print the tail and seek to the end
	return func() string {
		time.Sleep(500 * time.Millisecond) // a few poll ticks
		cancel()
		if err := <-errCh; err != nil {
			t.Fatalf("streamLogFile: %v", err)
		}
		return out.String()
	}
}

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

// A rename of the followed log must not be blocked by the follower. On Windows
// this fails with ERROR_SHARING_VIOLATION unless the file was opened with
// FILE_SHARE_DELETE; on other systems it passes trivially.
func TestStreamLogFile_DoesNotBlockRenameOrDelete(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stop := followFor(t, logPath, 1)
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatalf("rename of a followed log failed: %v", err)
	}
	if err := os.Remove(logPath + ".1"); err != nil {
		t.Fatalf("delete of a followed log failed: %v", err)
	}
	stop()
}

// After the log is renamed away and a new file is created at the same path
// (rotation), the follower must finish the old file and then pick up the new one.
func TestStreamLogFile_FollowsRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stop := followFor(t, logPath, 1)

	// the old file gets a last line, then is rotated away
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("last of old\n")
	f.Close()
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("first of new\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got := stop()
	for _, want := range []string{"last of old", "first of new"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Index(got, "last of old") > strings.Index(got, "first of new") {
		t.Fatalf("old file's last line must come before the new file: %q", got)
	}
}

// A truncated log is followed from its start again.
func TestStreamLogFile_FollowsTruncate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	if err := os.WriteFile(logPath, []byte("a long first line of text\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stop := followFor(t, logPath, 1)
	if err := os.Truncate(logPath, 0); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("after truncate\n")
	f.Close()
	if got := stop(); !strings.Contains(got, "after truncate") {
		t.Fatalf("missing the line written after truncate: %q", got)
	}
}
