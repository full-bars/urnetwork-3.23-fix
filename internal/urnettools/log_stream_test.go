package urnettools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
