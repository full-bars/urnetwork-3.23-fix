package connect

import (
	"net/netip"
	"testing"
)

func TestBlockerDefaultMalformedFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index string
		tails string
		count int
	}{
		{"missing index", "", "", 1},
		{"truncated index", string(make([]byte, blockerHostBucketBytes-1)), "", 1},
		{"missing tail", string(make([]byte, blockerHostBucketBytes)), "", 1},
		{"empty table", string(make([]byte, blockerHostBucketBytes)), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := newDefaultBlockerWithData(blockerTestPepper, tc.index, tc.tails, tc.count, "", 0, "", 0)
			if err == nil || b != nil {
				t.Fatal("invalid default data must return an error and no blocker")
			}
			closed := blockerOrFailClosed(b, err)
			closed.SetEnabled(false)
			if !closed.Enabled() || !closed.BlockHost("example.com") || !closed.BlockIp(netip.MustParseAddr("192.0.2.1")) {
				t.Fatal("invalid data must block even when disabled")
			}
			if got, ok := closed.(interface{ Error() error }); !ok || got.Error() != err {
				t.Fatal("failure must expose the original error")
			}
		})
	}
}
