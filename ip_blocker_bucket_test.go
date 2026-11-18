package connect

import (
	"encoding/binary"
	"slices"
	"testing"
)

func blockerBucketFixture(keys []uint64) (string, string) {
	index := make([]byte, blockerHostBucketBytes)
	tails := make([]byte, 0, len(keys)*blockerHostTailLen)
	pos := 0
	for bucket := 0; bucket < 256; bucket++ {
		binary.BigEndian.PutUint32(index[bucket*4:], uint32(pos))
		for pos < len(keys) && int(keys[pos]>>(8*blockerHostTailLen)) == bucket {
			for i := blockerHostTailLen - 1; i >= 0; i-- {
				tails = append(tails, byte(keys[pos]>>(8*i)))
			}
			pos++
		}
	}
	binary.BigEndian.PutUint32(index[256*4:], uint32(pos))
	return string(index), string(tails)
}

func TestBlockerBucketRoundTrip(t *testing.T) {
	keys := []uint64{0x000000000001, 0x000000000002, 0x810102030405, 0xff0102030405}
	index, tails := blockerBucketFixture(keys)
	table, err := decodeBlockerHostBuckets(index, tails, len(keys), blockerPepperLen)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if !table.contains(key) {
			t.Errorf("missing %012x", key)
		}
	}
	for _, key := range []uint64{0, 0x010000000001, 0x810102030406, 0xfe0102030405, 1 << 48} {
		if table.contains(key) {
			t.Errorf("unexpected match %012x", key)
		}
	}
}

func TestBlockerBucketMalformed(t *testing.T) {
	index, tails := blockerBucketFixture([]uint64{1, 2})
	change := func(offset int, value uint32) string {
		b := []byte(index)
		binary.BigEndian.PutUint32(b[offset*4:], value)
		return string(b)
	}
	cases := []struct {
		name, index, tails string
		count, extra       int
	}{
		{"empty index", "", tails, 2, 0},
		{"truncated index", index[:len(index)-1], tails, 2, 0},
		{"truncated tail", index, tails[:len(tails)-1], 2, 0},
		{"bad first offset", change(0, 1), tails, 2, 0},
		{"decreasing offset", change(2, 1), tails, 2, 0},
		{"out of range", change(1, 3), tails, 2, 0},
		{"bad end offset", change(256, 1), tails, 2, 0},
		{"unsorted tails", index, tails[5:] + tails[:5], 2, 0},
		{"duplicate tails", index, tails[:5] + tails[:5], 2, 0},
		{"negative count", index, tails, -1, 0},
		{"oversize", index, tails, 2, blockerMaxEmbeddedBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeBlockerHostBuckets(tc.index, tc.tails, tc.count, tc.extra); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := decodeBlockerHostBuckets(index, tails, 2, blockerMaxEmbeddedBytes-len(index)-len(tails)); err != nil {
		t.Fatalf("exact size limit: %v", err)
	}
}

func TestBlockerBucketHostLookup(t *testing.T) {
	names := []string{"ads.example.com", "tracker.net", "sub.example.org"}
	keys := make([]uint64, 0, len(names))
	for _, name := range names {
		keys = append(keys, blockerTestHostKey(blockerTestPepper, name))
	}
	slices.Sort(keys)
	index, tails := blockerBucketFixture(keys)
	b, err := newBucketBlockerWithData(blockerTestPepper, index, tails, len(keys), "", 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	b.SetEnabled(true)
	for _, name := range append(names, "a.ads.example.com") {
		if !b.BlockHost(name) {
			t.Errorf("missing blocked name %q", name)
		}
	}
	if b.BlockHost("example.net") {
		t.Fatal("unexpected block")
	}
	if _, err := newBucketBlockerWithData(blockerTestPepper, "", tails, len(keys), "", 0, "", 0); err == nil {
		t.Fatal("malformed bucket index accepted")
	}
}

func TestBlockerBucketFormatDrift(t *testing.T) {
	if blockerHostRecordLen != 6 || blockerHostTailLen != 5 || blockerHostBucketBytes != 257*4 {
		t.Fatal("blocker bucket format changed: update decoder, generator and format tests together")
	}
	index, tails := blockerBucketFixture(nil)
	if _, err := decodeBlockerHostBuckets(index, tails, 0, 0); err != nil {
		t.Fatal(err)
	}
}
