package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The provider writes state, snapshots and event logs under ~/.urnetwork, and
// several code paths log through importantLogf (which also appends to the disk
// events.log). Tests that forget withTempHome used to write those into the
// developer's REAL home: stray client-JWT snapshots, [proxy][url] cap eviction
// lines and [oomcap] acknowledgements in a live provider's events.log.
//
// Point HOME at a throwaway directory for the whole test binary, before any test
// runs, so an un-isolated test can no longer touch real state. withTempHome still
// gives each test its own directory on top of this.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "urnetwork-provider-test-home-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir)
	// globalClientJWTStore is built at package init from the real home, before
	// TestMain runs; replace it.
	globalClientJWTStore = newClientJWTStore(filepath.Join(dir, ".urnetwork", ".client_jwts.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
