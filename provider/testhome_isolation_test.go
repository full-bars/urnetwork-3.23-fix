package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// globalClientJWTStore is built at package init from the real HOME, so
// redirecting HOME per test was not enough: any reload that touched the store
// read the developer's real stored identities and wrote snapshot files into
// their real ~/.urnetwork. withTempHome must point the store into the temp
// home as well, and put the real one back afterwards.
func TestWithTempHomeIsolatesTheClientJWTStore(t *testing.T) {
	before := globalClientJWTStore
	var dir string
	t.Run("inside a temp-home test", func(t *testing.T) {
		dir = withTempHome(t)
		if globalClientJWTStore == before {
			t.Fatalf("the process-wide JWT store was not replaced")
		}
		want := filepath.Join(dir, ".urnetwork") + string(filepath.Separator)
		if !strings.HasPrefix(globalClientJWTStore.path, want) {
			t.Fatalf("JWT store path %q is outside the temp home %q: reload would read and write the developer's real identities", globalClientJWTStore.path, dir)
		}
	})
	if globalClientJWTStore != before {
		t.Fatalf("withTempHome must restore the previous JWT store on cleanup")
	}
}
