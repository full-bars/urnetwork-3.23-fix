//go:build windows

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A log below a path longer than MAX_PATH must open. syscall.CreateFile does
// not extend long paths the way os.Open does, so openFollowFile prefixes them.
// On a host with long paths enabled the OS accepts the plain path too, so this
// passes there even without the prefix; TestLongPath pins the prefixing itself.
func TestOpenFollowFile_PathLongerThanMaxPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 50))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create a long path here: %v", err)
	}
	path := filepath.Join(dir, "provider.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Skipf("cannot write a long path here: %v", err)
	}
	f, err := openFollowFile(path)
	if err != nil {
		t.Fatalf("a path of %d chars must open: %v", len(path), err)
	}
	defer f.Close()
	buf := make([]byte, 6)
	if _, err := f.Read(buf); err != nil || string(buf) != "hello\n" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

func TestLongPath(t *testing.T) {
	if got := longPath(`C:\short`); got != `C:\short` {
		t.Fatalf("short path changed: %q", got)
	}
	long := `C:\` + strings.Repeat(`a\`, 150) + "f.log"
	if got := longPath(long); !strings.HasPrefix(got, `\\?\C:\`) {
		t.Fatalf("long path not prefixed: %q", got[:20])
	}
	unc := `\\srv\share\` + strings.Repeat(`a\`, 150) + "f.log"
	if got := longPath(unc); !strings.HasPrefix(got, `\\?\UNC\srv\share\`) {
		t.Fatalf("UNC path not converted: %q", got[:24])
	}
	pre := `\\?\C:\` + strings.Repeat(`a\`, 150)
	if got := longPath(pre); got != pre {
		t.Fatal("an already prefixed path must be left alone")
	}
}
