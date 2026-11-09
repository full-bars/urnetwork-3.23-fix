//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeUnit points the unit rewrite at a temp file and reports Type= by
// reading that file, so migrate and demote run end to end without systemd.
func fakeUnit(t *testing.T, content string) (Provider, string, *int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork.service")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reloads := 0

	origType, origPath, origReload := unitTypeFunc, unitFilePathFunc, daemonReloadFunc
	t.Cleanup(func() {
		unitTypeFunc, unitFilePathFunc, daemonReloadFunc = origType, origPath, origReload
	})
	unitTypeFunc = func(Provider) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Type="); ok {
				return v, nil
			}
		}
		return "simple", nil
	}
	unitFilePathFunc = func(Provider) (string, error) { return path, nil }
	daemonReloadFunc = func(Provider) error { reloads++; return nil }

	return Provider{Unit: "urnetwork.service", User: "provider"}, path, &reloads
}

func readUnit(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const simpleUnit = `[Service]
# Type=simple (the default), deliberately NOT Type=notify.
Type=simple
ExecStart=/usr/bin/urnetwork provide
Restart=on-failure
`

func TestDemoteNotifyReloadToSimple(t *testing.T) {
	p, path, reloads := fakeUnit(t, "[Service]\nType=notify-reload\nExecStart=/usr/bin/urnetwork provide\n")
	changed, err := demoteUnitToSimple(p)
	if err != nil || !changed {
		t.Fatalf("demote notify-reload: changed=%v err=%v", changed, err)
	}
	if got := readUnit(t, path); !strings.Contains(got, "\nType=simple\n") || strings.Contains(got, "Type=notify-reload") {
		t.Fatalf("unit not demoted: %s", got)
	}
	if *reloads != 1 {
		t.Fatalf("daemon-reload ran %d times, want 1", *reloads)
	}
}

func TestRewriteUnitContentToSimple(t *testing.T) {
	migrated, changed := rewriteUnitContent(simpleUnit)
	if !changed {
		t.Fatal("rewriteUnitContent did not migrate a Type=simple unit")
	}
	back, changed := rewriteUnitContentToSimple(migrated)
	if !changed {
		t.Fatal("rewriteUnitContentToSimple did not change a Type=notify unit")
	}
	if strings.Contains(back, "\nType=notify") || !strings.Contains(back, "\nType=simple\n") {
		t.Errorf("unit not restored to Type=simple:\n%s", back)
	}
	if !strings.Contains(back, "# Type=simple (the default), deliberately NOT Type=notify.") {
		t.Errorf("comment line was rewritten:\n%s", back)
	}
	if _, changed := rewriteUnitContentToSimple(simpleUnit); changed {
		t.Error("rewriteUnitContentToSimple changed a unit that is already Type=simple")
	}
}

// TestReconcileUnitTypeFollowsBinary: the unit is promoted only for a binary
// that sends READY=1 at startup, and demoted again for an older binary or an
// unreadable version, so a notify unit is never left paired with a binary
// that cannot signal readiness.
func TestReconcileUnitTypeFollowsBinary(t *testing.T) {
	p, path, reloads := fakeUnit(t, simpleUnit)

	migrated, err := reconcileUnitTypeForBinary(p, "v3.23.0-fix.31.1")
	if err != nil || !migrated {
		t.Fatalf("31.1 on a simple unit: migrated=%v err=%v, want migrated", migrated, err)
	}
	if got := readUnit(t, path); !strings.Contains(got, "\nType=notify\n") {
		t.Fatalf("unit not migrated:\n%s", got)
	}
	if bak := readUnit(t, path+".bak"); bak != simpleUnit {
		t.Errorf("backup is not the original unit:\n%s", bak)
	}

	migrated, err = reconcileUnitTypeForBinary(p, "v3.23.0-fix.30.9")
	if err != nil || migrated {
		t.Fatalf("30.9 downgrade: migrated=%v err=%v, want demotion without error", migrated, err)
	}
	if got := readUnit(t, path); !strings.Contains(got, "\nType=simple\n") || strings.Contains(got, "\nType=notify") {
		t.Fatalf("downgrade left the unit Type=notify:\n%s", got)
	}

	if _, err := reconcileUnitTypeForBinary(p, "v3.23.0-fix.31.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileUnitTypeForBinary(p, ""); err != nil {
		t.Fatal(err)
	}
	if got := readUnit(t, path); !strings.Contains(got, "\nType=simple\n") {
		t.Fatalf("unreadable version left the unit Type=notify:\n%s", got)
	}

	if *reloads != 4 {
		t.Errorf("daemon-reload ran %d times, want 4 (one per rewrite)", *reloads)
	}

	// Already simple and an old binary: nothing to do.
	if _, err := reconcileUnitTypeForBinary(p, "v3.23.0-fix.30.9"); err != nil {
		t.Fatal(err)
	}
	if *reloads != 4 {
		t.Errorf("no-op demotion reloaded systemd")
	}
}

// TestMigrateUnitRefusesSymlinkedTemp: urnet-tools runs as root and a user
// unit lives in a directory the provider user controls. A symlink planted
// at the temp path must not redirect the write.
func TestMigrateUnitRefusesSymlinkedTemp(t *testing.T) {
	p, path, _ := fakeUnit(t, simpleUnit)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Fatal(err)
	}

	if migrated, err := migrateUnitToNotify(p); err == nil || migrated {
		t.Fatalf("migrate through a symlinked temp: migrated=%v err=%v, want refusal", migrated, err)
	}
	if got := readUnit(t, victim); got != "untouched" {
		t.Fatalf("migration wrote through the symlink: victim now %q", got)
	}
	if got := readUnit(t, path); got != simpleUnit {
		t.Errorf("unit changed despite the refused write:\n%s", got)
	}
}

// TestMigrateUnitRefusesSymlinkedUnitFile: reading through a symlinked unit
// as root would copy an arbitrary root-only file into the world-readable
// .bak.
func TestMigrateUnitRefusesSymlinkedUnitFile(t *testing.T) {
	p, path, _ := fakeUnit(t, simpleUnit)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(simpleUnit+"# secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}

	if migrated, err := migrateUnitToNotify(p); err == nil || migrated {
		t.Fatalf("migrate through a symlinked unit: migrated=%v err=%v, want refusal", migrated, err)
	}
	if _, err := os.Lstat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf(".bak was written from a symlinked unit (lstat err=%v)", err)
	}
}
