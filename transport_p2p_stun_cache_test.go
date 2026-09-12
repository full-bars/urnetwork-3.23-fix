package connect

import (
	"testing"
	"time"
)

func TestSTUNHealthyUnknownURL(t *testing.T) {
	// A URL never seen should be healthy.
	if !stunURLHealthy("stun:example.com:3478") {
		t.Fatal("unknown URL should be healthy")
	}
}

func TestSTUNHealthyRecentlyFailed(t *testing.T) {
	url := "stun:fail1.example.com:3478"
	markSTUNFailed([]string{url})
	if stunURLHealthy(url) {
		t.Fatal("recently-failed URL should be unhealthy")
	}
	// Clean up.
	stunFailureCache.Delete(url)
}

func TestSTUNHealthyAfterTTL(t *testing.T) {
	url := "stun:expire-test.example.com:3478"
	// Insert a failure with a timestamp older than the TTL.
	stunFailureCache.Store(url, time.Now().Add(-stunCacheTTL-time.Second))
	if !stunURLHealthy(url) {
		t.Fatal("expired entry should be healthy again")
	}
	stunFailureCache.Delete(url)
}

func TestSTUNFilterRemovesFailed(t *testing.T) {
	failed1 := "stun:fail1.example.com:3478"
	failed2 := "stun:fail2.example.com:3478"
	good := "stun:stun.l.google.com:19302"
	markSTUNFailed([]string{failed1, failed2})

	urls := []string{failed1, failed2, good}
	healthy := filterSTUNURLs(urls)
	for _, u := range healthy {
		if u == failed1 || u == failed2 {
			t.Fatalf("filtered list should not contain %s", u)
		}
	}
	// Clean up.
	stunFailureCache.Delete(failed1)
	stunFailureCache.Delete(failed2)
}

func TestSTUNFilterPreservesAtLeastOne(t *testing.T) {
	// Fail ALL URLs including Google ones.
	all := []string{
		"stun:stun.l.google.com:19302",
		"stun:stun1.l.google.com:19302",
		"stun:other.example.com:3478",
	}
	markSTUNFailed(all)
	healthy := filterSTUNURLs(all)
	if len(healthy) == 0 {
		t.Fatal("filterSTUNURLs should always return at least one URL")
	}
	// Should fall back to a Google URL.
	found := false
	for _, u := range healthy {
		if u == "stun:stun.l.google.com:19302" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback should include a Google STUN URL, got %v", healthy)
	}
	// Clean up.
	for _, u := range all {
		stunFailureCache.Delete(u)
	}
}
