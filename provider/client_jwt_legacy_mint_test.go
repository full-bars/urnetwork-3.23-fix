package main

import (
	"testing"
	"time"
)

// An entry with no mint time predates the field. Pruning it as stale would
// discard every identity in an existing store on the first start after an
// upgrade, so the operator pays a full re-auth for the whole proxy list.
func TestPruneStaleEntriesKeepsEntriesWithoutMintedAt(t *testing.T) {
	now := time.Now()
	entries := map[string]clientJWTEntry{
		"legacy": {ByClientJWT: "legacy-jwt", ClientID: "c-legacy"},
		"fresh":  {ByClientJWT: "fresh-jwt", ClientID: "c-fresh", MintedAt: now},
		"stale":  {ByClientJWT: "stale-jwt", ClientID: "c-stale", MintedAt: now.Add(-clientJWTStaleAfter - time.Hour)},
	}

	pruned := pruneStaleEntries(entries)

	if _, ok := pruned["legacy"]; !ok {
		t.Fatal("an entry with no MintedAt was pruned; an upgrade would force every proxy to re-auth")
	}
	if _, ok := pruned["fresh"]; !ok {
		t.Fatal("a fresh entry was pruned")
	}
	if _, ok := pruned["stale"]; ok {
		t.Fatal("an entry older than the stale threshold survived")
	}
	if len(pruned) != 2 {
		t.Fatalf("kept %d entries, want 2", len(pruned))
	}
}
