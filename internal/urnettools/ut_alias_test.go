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

func TestRemoveInstalledUTAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(tool, []byte("tool"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := ensureUTSymlink(tool); err != nil {
		t.Fatal(err)
	}
	if err := removeInstalledUTAlias(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "ut")); !os.IsNotExist(err) {
		t.Fatalf("alias remains: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ut"), []byte("foreign"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := removeInstalledUTAlias(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ut")); err != nil || string(b) != "foreign" {
		t.Fatalf("foreign alias changed: %q %v", b, err)
	}
}

func TestUTCopyOwnership(t *testing.T) {
	dir := t.TempDir()
	tool, alias := filepath.Join(dir, "urnet-tools.exe"), filepath.Join(dir, "ut.exe")
	if err := os.WriteFile(tool, []byte("one"), 0755); err != nil {
		t.Fatal(err)
	}
	if !utCopyOwned(tool, alias) {
		t.Fatal("missing alias should be creatable")
	}
	if err := os.WriteFile(alias, []byte("one"), 0755); err != nil {
		t.Fatal(err)
	}
	if !utCopyOwned(tool, alias) {
		t.Fatal("matching copy should be owned")
	}
	if err := os.WriteFile(alias, []byte("other"), 0755); err != nil {
		t.Fatal(err)
	}
	if utCopyOwned(tool, alias) {
		t.Fatal("foreign copy must not be owned")
	}
}
