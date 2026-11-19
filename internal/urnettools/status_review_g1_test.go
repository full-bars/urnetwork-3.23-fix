package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A state dir can belong to another user while status runs elevated. A symlink
// there must not redirect the cache write to another file.
func TestStatusUpdateCacheWriteDoesNotFollowASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, statusUpdateCacheFile)); err != nil {
		t.Fatal(err)
	}
	writeStatusUpdateCache(dir, statusUpdateState{At: time.Now(), Tag: "v1"})
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the write followed the symlink and changed the target: %q", b)
	}
	info, err := os.Lstat(filepath.Join(dir, statusUpdateCacheFile))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the cache should be a regular file now, err=%v mode=%v", err, info)
	}
	if s, ok := readStatusUpdateCache(dir); !ok || s.Tag != "v1" {
		t.Fatalf("round trip failed: %+v %v", s, ok)
	}
}

func TestStatusUpdateCacheReadIgnoresASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(target, []byte(`{"tag":"vX","at":"`+time.Now().Format(time.RFC3339Nano)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, statusUpdateCacheFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok := readStatusUpdateCache(dir); ok {
		t.Fatal("a symlinked cache must not be read")
	}
}

func TestStatusExtenderRowShowsAnUnavailableOrMissingRole(t *testing.T) {
	row := func(e ExtenderReading) statusRow {
		for _, r := range makeStatusRows(Provider{}, nil, e, nil) {
			if r.Name == "Extender" {
				return r
			}
		}
		t.Fatal("no extender row")
		return statusRow{}
	}
	if r := row(ExtenderReading{Known: true, Running: true, RoleState: "serving"}); r.Text != "active" || r.Level != statusOK {
		t.Fatalf("serving: %+v", r)
	}
	if r := row(ExtenderReading{Known: true, Running: true, RoleState: "tcp_unavailable"}); r.Text != "unavailable" || r.Level != statusWarn {
		t.Fatalf("a role that cannot bind tcp must not read active: %+v", r)
	}
	if r := row(ExtenderReading{Known: true, Setting: "on", Running: false}); r.Text != "not running" || r.Level != statusWarn {
		t.Fatalf("switched on but not running must not read off: %+v", r)
	}
	if r := row(ExtenderReading{Known: true, Setting: "off", Running: false}); r.Text != "off" || r.Level != statusOK {
		t.Fatalf("switched off: %+v", r)
	}
}

// With one accessible provider and a pid for another one, the pid wins.
func TestSelectTargetKeepsAPIDWhenOneProviderIsAccessible(t *testing.T) {
	orig := isPrivileged
	isPrivileged = func() bool { return false }
	defer func() { isPrivileged = orig }()
	providers := []Provider{
		{User: currentUserName(), PID: 1111, Unit: "urnetwork.service", Running: true},
		{User: "someone-else", PID: 2222, Unit: "urnetwork-beta.service", Running: true},
	}
	p, narrowed, err := selectTargetOrSoleAccessible(providers, Target{PID: 2222}, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 2222 || narrowed {
		t.Fatalf("the pid was ignored: got pid %d narrowed=%v", p.PID, narrowed)
	}
}

func TestBriefExitIsNilWhenHealthy(t *testing.T) {
	if err := briefExit(0); err != nil {
		t.Fatalf("a healthy status must return no error, got %v", err)
	}
	if err := briefExit(2); err == nil || err.(briefExitError).ExitCode() != 2 {
		t.Fatalf("a failing status keeps its exit code: %v", err)
	}
}

// A file swapped in after the inspection must not be read: readRegularFile
// refuses a symlink and reads a regular file.
func TestReadRegularFileRefusesASymlinkAndReadsAFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := readRegularFile(real)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("a regular file reads: %q %v", b, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(link); err == nil {
		t.Fatal("a symlink must be refused")
	}
	if _, err := readRegularFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file is an error")
	}
}
