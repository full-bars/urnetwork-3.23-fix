package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUTSymlinkLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "urnet-tools")
	alias := filepath.Join(dir, "ut")
	if err := os.WriteFile(tool, []byte("tool"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensureUTSymlink(tool); err != nil {
		t.Fatal(err)
	}
	if err := ensureUTSymlink(tool); err != nil {
		t.Fatal(err)
	}
	if !utAliasOwned(tool, alias) {
		t.Fatal("link not owned")
	}
	if err := removeUTSymlink(tool, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("foreign"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensureUTSymlink(tool); err == nil {
		t.Fatal("accepted foreign file")
	}
	if err := removeUTSymlink(tool, alias); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(alias); err != nil || string(b) != "foreign" {
		t.Fatalf("foreign file changed: %q %v", b, err)
	}
}
