package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Process-wide proxy state that a test can dirty must not leak into the next
// test. The trim tests shed proxies (which parks them in the global failure
// history) and change the acknowledged trim cap; a later test reusing the same
// proxy keys then saw them "still in backoff" or an already-acknowledged cap,
// which made results depend on test order. withTempHome resets both.
func TestWithTempHomeResetsSharedProxyState(t *testing.T) {
	const key = "10.9.9.9:1080"
	globalProxyFailureHistory.SetBackoffUntil(key, time.Now().Add(time.Hour))
	trimCapSeen.Store(1234)
	t.Cleanup(func() { globalProxyFailureHistory.SetBackoffUntil(key, time.Time{}); trimCapSeen.Store(0) })

	t.Run("a fresh temp-home test starts clean", func(t *testing.T) {
		withTempHome(t)
		if !globalProxyFailureHistory.Eligible(key, time.Now()) {
			t.Fatalf("a backoff set by an earlier test leaked into this one")
		}
		if got := trimCapSeen.Load(); got != 0 {
			t.Fatalf("acknowledged trim cap %d leaked into this test", got)
		}
	})
}

// A test that forgets withTempHome must still not reach the developer's real
// home: TestMain points HOME at a throwaway directory for the whole binary.
func TestPackageHomeIsNotTheDevelopersRealHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(home, "urnetwork-provider-test-home-") {
		t.Fatalf("HOME is %q: tests would read and write the developer's real ~/.urnetwork", home)
	}
	if !strings.HasPrefix(globalClientJWTStore.path, home) {
		t.Fatalf("the JWT store %q is outside the test home %q", globalClientJWTStore.path, home)
	}
}
