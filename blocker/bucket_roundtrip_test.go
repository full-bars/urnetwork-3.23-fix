package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestGeneratedBucketFixture is invoked by the root package's round trip test.
func TestGeneratedBucketFixture(t *testing.T) {
	path := os.Getenv("BLOCKER_ROUNDTRIP_FIXTURE")
	if path == "" {
		return
	}
	blocked := []string{"ads.example.com", "tracker.example.net", "metrics.example.org"}
	names := map[string]bool{}
	for _, name := range blocked {
		names[name] = true
	}
	resolvers, addrs := resolverExclusions()
	if len(addrs) == 0 {
		t.Fatal("default resolver addresses were not collected")
	}
	// Current defaults are all IP literals; exercise the same allow-set path
	// with a synthetic resolver hostname without changing those defaults.
	resolvers["resolver.example.test"] = true
	var excluded []string
	for name := range resolvers {
		names[name] = true
		excluded = append(excluded, name)
	}
	sort.Strings(excluded)
	if len(excluded) == 0 {
		t.Fatal("no resolver names available to exercise exclusions")
	}
	subtractAllow(names, resolvers)
	compressSuffixes(names)
	pepper := strings.Repeat("a", 32)
	source := emit("standard", pepper, hashHosts(pepper, names), nil, nil, nil)
	fixture := struct {
		Source   string
		Blocked  []string
		Excluded []string
	}{string(source), blocked, excluded}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedBucketPackingAndBudget(t *testing.T) {
	keys := []uint64{0x000000000001, 0x010102030405, 0x010102030406, 0xff0102030405}
	index, tails := packHostHashes(keys)
	if len(index) != hostBucketBytes || len(tails) != len(keys)*hostTailLen {
		t.Fatalf("unexpected lengths: index=%d tails=%d", len(index), len(tails))
	}
	for bucket, want := range map[int]uint32{0: 0, 1: 1, 2: 3, 255: 3, 256: 4} {
		if got := binary.BigEndian.Uint32(index[bucket*4:]); got != want {
			t.Errorf("bucket %d offset = %d, want %d", bucket, got, want)
		}
	}
	for _, tc := range []struct {
		size, budget int
		valid        bool
	}{
		{maxEmbeddedBytes, maxEmbeddedBytes, true},
		{maxEmbeddedBytes + 1, maxEmbeddedBytes + 100, false},
		{100, 99, false},
		{100, 100, true},
	} {
		if (checkEmbeddedSize(tc.size, tc.budget) == nil) != tc.valid {
			t.Errorf("size=%d budget=%d: want valid=%t", tc.size, tc.budget, tc.valid)
		}
	}
}
