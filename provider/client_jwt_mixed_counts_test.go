package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJWTStoreMixedBatchReportsAllChanges(t *testing.T) {
	s := newClientJWTStore(filepath.Join(t.TempDir(), "store"))
	e := clientJWTEntry{ByClientJWT: "old", MintedAt: time.Now()}
	if err := s.Put("old", e); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("replace", e); err != nil {
		t.Fatal(err)
	}
	changed := e
	changed.ByClientJWT = "new"
	out := captureTlog(t, func() {
		if err := s.flushBatchLocked(map[string]clientJWTEntry{"new": e, "replace": changed}, []string{"old"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "added=1") || !strings.Contains(out, "removed=1") || !strings.Contains(out, "replaced=1") {
		t.Fatalf("missing mixed change counts: %s", out)
	}
}
