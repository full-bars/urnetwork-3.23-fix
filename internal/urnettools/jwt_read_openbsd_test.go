package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadJWTFileOpenBSD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJWTFile(path); err == nil {
		t.Fatal("FIFO accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a.b.c"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := readJWTFile(path); err != nil || string(b) != "a.b.c" {
		t.Fatalf("regular file: %q, %v", b, err)
	}
	link := filepath.Join(dir, "jwt-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readJWTFile(link); err == nil {
		t.Fatal("symlinked JWT accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxJWTBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJWTFile(path); err == nil {
		t.Fatal("oversized file accepted")
	}
}
