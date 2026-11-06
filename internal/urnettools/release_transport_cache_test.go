package urnettools

import (
	"testing"
	"time"
)

// The fixture swaps the transport, so a release cached before it (or by a dry
// run under it) must neither bypass the transport nor outlive the test.
func TestWithRecordingTransportIsolatesReleaseCache(t *testing.T) {
	seeded := &releaseInfo{Tag: "v0.0.0-seeded", ProviderDigest: "abc"}
	seededAt := time.Now().Add(-time.Minute)
	latestReleaseMu.Lock()
	origInfo, origTime := cachedLatest, cachedLatestTime
	cachedLatest, cachedLatestTime = seeded, seededAt
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		latestReleaseMu.Lock()
		cachedLatest, cachedLatestTime = origInfo, origTime
		latestReleaseMu.Unlock()
	})

	t.Run("fixture", func(t *testing.T) {
		withRecordingTransport(t, `[{"tag_name":"v9.9.9-canned","assets":[{"name":"urnetwork-provider-v9.9.9-canned.tar.gz","digest":"sha256:abc"}]}]`)
		latestReleaseMu.Lock()
		cleared := cachedLatest == nil
		latestReleaseMu.Unlock()
		if !cleared {
			t.Fatalf("a release cached before the fixture would bypass the recording transport")
		}
		info, err := latestRelease()
		if err != nil {
			t.Fatalf("latestRelease: %v", err)
		}
		if info.Tag != "v9.9.9-canned" {
			t.Fatalf("tag = %q, want the canned tag", info.Tag)
		}
	})

	latestReleaseMu.Lock()
	gotInfo, gotTime := cachedLatest, cachedLatestTime
	latestReleaseMu.Unlock()
	if gotInfo != seeded || !gotTime.Equal(seededAt) {
		t.Fatalf("cache after the fixture = %v @ %v, want the seeded %v @ %v", gotInfo, gotTime, seeded, seededAt)
	}
}
