//go:build linux

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteStateFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real_file")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink pointing outside the state dir
	link := filepath.Join(dir, "evil_link")
	if err := os.Symlink("/etc/shadow", link); err != nil {
		t.Fatal(err)
	}

	err := writeStateFile(dir, "evil_link", []byte("pwned"), 0o644)
	if err == nil {
		t.Fatal("writeStateFile should refuse a symlink target")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should mention symlink, got: %v", err)
	}
}

func TestWriteStateFileWritesCorrectly(t *testing.T) {
	dir := t.TempDir()
	data := []byte("test state content\n")
	if err := writeStateFile(dir, "node_name", data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "node_name"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}
	// Verify mode
	info, err := os.Stat(filepath.Join(dir, "node_name"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("perm: got %o, want 0644", info.Mode().Perm())
	}
}

func TestWriteStateFileOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	// Create with wrong perm
	if err := writeStateFile(dir, "file", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Overwrite with correct perm — Fchmod should enforce it
	if err := writeStateFile(dir, "file", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content: got %q, want 'new'", got)
	}
	info, _ := os.Stat(filepath.Join(dir, "file"))
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("perm after overwrite: got %o, want 0644", info.Mode().Perm())
	}
}

func TestValidateTag(t *testing.T) {
	good := []string{"v3.23.0-fix.30.9", "latest", "abc123", "v1.0.0+build"}
	for _, tag := range good {
		if err := validateTag(tag); err != nil {
			t.Errorf("validateTag(%q) unexpected error: %v", tag, err)
		}
	}
	bad := []string{
		"../../../etc/shadow", // path traversal
		"../../rm -rf /",      // traversal
		"",                    // empty
		"tag/with/slash",      // slash
		"tag;rm -rf",          // shell injection
		"tag space",           // space
	}
	for _, tag := range bad {
		if err := validateTag(tag); err == nil {
			t.Errorf("validateTag(%q) should have failed", tag)
		}
	}
}
