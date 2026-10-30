package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCopyFileRefusesExistingDestination(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			if kind != "file" && runtime.GOOS == "windows" {
				t.Skip("symlinks require privileges on Windows")
			}
			dir := t.TempDir()
			src, dst, target := filepath.Join(dir, "src"), filepath.Join(dir, "backup"), filepath.Join(dir, "target")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				if err := os.WriteFile(dst, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "symlink" {
					if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			err := copyFile(src, dst)
			if !os.IsExist(err) {
				t.Errorf("copyFile error = %v, want existence refusal", err)
			}
			check := dst
			if kind != "file" {
				check = target
			}
			got, readErr := os.ReadFile(check)
			if kind == "dangling symlink" {
				if !os.IsNotExist(readErr) {
					t.Errorf("dangling target was created: %q, %v", got, readErr)
				}
			} else if readErr != nil || string(got) != "keep" {
				t.Errorf("existing content changed: %q, %v", got, readErr)
			}
		})
	}
}

// The self-update backup is taken with os.Link, which fails with EEXIST when
// the path exists. That branch was previously unreachable from a test, so the
// fallback could be reached without anything asserting it refuses the path.
func TestBackupToolBinaryRefusesExistingDestination(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "symlink" && runtime.GOOS == "windows" {
				t.Skip("symlinks require privileges on Windows")
			}
			dir := t.TempDir()
			src, dst, target := filepath.Join(dir, "src"), filepath.Join(dir, "backup"), filepath.Join(dir, "target")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				if err := os.WriteFile(dst, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			if err := backupToolBinary(src, dst); !os.IsExist(err) {
				t.Fatalf("backupToolBinary error = %v, want existence refusal", err)
			}
			check := dst
			if kind == "symlink" {
				check = target
			}
			if got, err := os.ReadFile(check); err != nil || string(got) != "keep" {
				t.Errorf("existing content changed: %q, %v", got, err)
			}
		})
	}
}
