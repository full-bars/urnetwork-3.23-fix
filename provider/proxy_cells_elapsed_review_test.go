package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyCellRatesByElapsedTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		delta   uint64
		bucket  uint8
		detail  uint64
	}{
		{"one second", time.Second, 4096, 2, 4096},
		{"two seconds", 2 * time.Second, 8192, 2, 8192},
		{"three seconds", 3 * time.Second, 12288, 2, 12288},
		{"ten seconds", 10 * time.Second, 40960, 2, 40960},
		{"long gap", 10 * time.Minute, 900000, 0, 0},
		{"zero elapsed", 0, 2048, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connect.ResetProxyHealthForTesting()
			defer connect.ResetProxyHealthForTesting()
			connect.RegisterProxy(0, "address", "address")
			bw := connect.RegisterProxyBandwidth(0)
			now := time.Unix(1000, 0)
			store := &proxyCellStore{}
			store.Cells(now)
			bw.TotalRx.Store(tc.delta)
			if tc.elapsed == 0 {
				store.raw = nil
			} // force a resample at the same timestamp
			cells := store.Cells(now.Add(tc.elapsed))
			detail := store.Detail(now.Add(tc.elapsed))
			if got := cells[2] & 7; got != tc.bucket {
				t.Errorf("bucket=%d want %d", got, tc.bucket)
			}
			if got := detail[0].IntervalBytes; got != tc.detail {
				t.Errorf("interval_bytes=%d want %d", got, tc.detail)
			}
		})
	}
}
