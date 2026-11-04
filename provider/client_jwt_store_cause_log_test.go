package main

import (
	"strings"
	"testing"
	"time"
)

func fixedEntry(jwt string) clientJWTEntry {
	return clientJWTEntry{
		ByClientJWT: jwt,
		ClientID:    testClientId,
		NetworkID:   "net-main",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
}

// Every distinct reason logs its first occurrence; repeats are held back for
// the interval and the next line reports how many were held back.
func TestJWTStoreWriteCauseLogIsRateLimitedPerReason(t *testing.T) {
	s := newJWTStoreForTest(t)
	clock := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s.logNow = func() time.Time { return clock }

	putP1 := func(jwt string) { s.Put("p1", fixedEntry(jwt)) } // one call site
	out := captureTlog(t, func() {
		for i := 0; i < 5; i++ { // same site, same kind of change: updated
			putP1("jwt-" + string(rune('a'+i)))
		}
		s.Put("p2", fixedEntry("jwt-x")) // a different reason: added, own first line
		s.Delete("p2")                   // yet another: delete / removed
		clock = clock.Add(2 * clientJWTWriteLogInterval)
		putP1("jwt-z") // same reason as the 3 held back
	})

	if got := strings.Count(out, "[jwt-store] write"); got != 5 {
		t.Fatalf("want 5 cause lines (add, first update, add p2, delete, update after interval), got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "suppressed=3") {
		t.Fatalf("the line after the interval must report the held-back repeats:\n%s", out)
	}
	if !strings.Contains(out, "op=delete") || !strings.Contains(out, "removed=1") {
		t.Fatalf("delete cause missing:\n%s", out)
	}
	if !strings.Contains(out, "client_jwt_store_cause_log_test.go:") {
		t.Fatalf("cause line should name the calling site:\n%s", out)
	}
}
