package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyCellStoreDetail(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()
	connect.RegisterProxy(2, "10.0.0.1:1080", "10.0.0.1:1080")
	connect.RegisterProxy(1, "10.0.0.2:1080", "10.0.0.2:1080")
	connect.NoteLiveProxyHealth("10.0.0.1:1080", "up", time.Time{})

	store := &proxyCellStore{prev: map[int]uint64{}}
	now := time.Now()

	// first call establishes the baseline (interval bytes 0)
	cells := store.Cells(now)
	if len(cells) != 8 {
		t.Fatalf("want 8 packed bytes for 2 proxies, got %d", len(cells))
	}
	detail := store.Detail(now)
	if len(detail) != 2 {
		t.Fatalf("want 2 detail records, got %d", len(detail))
	}
	byIndex := map[int]proxyCellDetail{}
	for _, d := range detail {
		byIndex[d.Index] = d
	}
	if byIndex[2].Addr != "10.0.0.1:1080" || byIndex[1].Addr != "10.0.0.2:1080" {
		t.Fatalf("detail addrs wrong: %+v", detail)
	}
	for _, d := range detail {
		if d.IntervalBytes != 0 {
			t.Fatalf("first sample should report zero interval bytes, got %d for proxy %d", d.IntervalBytes, d.Index)
		}
	}

	// advance counters and re-sample: interval = 512KiB rx + 256KiB tx
	bw := connect.RegisterProxyBandwidth(2)
	bw.TotalRx.Store(512 << 10)
	bw.TotalTx.Store(256 << 10)
	now2 := now.Add(time.Second)
	cells2 := store.Cells(now2)
	detail2 := store.Detail(now2)
	byIndex = map[int]proxyCellDetail{}
	for _, d := range detail2 {
		byIndex[d.Index] = d
	}
	wantBytes := uint64(512<<10) + uint64(256<<10)
	if byIndex[2].IntervalBytes != wantBytes {
		t.Fatalf("proxy 0 interval bytes = %d, want %d", byIndex[2].IntervalBytes, wantBytes)
	}
	// the bucket for ~768 KiB in the interval must be >= 4 (512KiB..4MiB band)
	bucket := cells2[6] & 0x07
	if bucket < 4 {
		t.Fatalf("proxy 0 bucket = %d, want >= 4 for a 768 KiB interval", bucket)
	}
}

func TestProxyCellStoreCacheValidOneSecond(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()
	connect.RegisterProxy(0, "10.0.0.1:1080", "10.0.0.1:1080")

	store := &proxyCellStore{prev: map[int]uint64{}}
	now := time.Now()
	first := store.Cells(now)
	// same-second call must return the cached slice, not rescan
	second := store.Cells(now.Add(500 * time.Millisecond))
	if &first[0] != &second[0] {
		t.Fatal("cached cells should be returned within the TTL")
	}
	// after the TTL it recomputes
	third := store.Cells(now.Add(time.Second + time.Millisecond))
	if len(third) != len(first) {
		t.Fatalf("recomputed cells length %d != %d", len(third), len(first))
	}
}

func TestProxyCellDetailJSONShape(t *testing.T) {
	detail := proxyCellDetail{Index: 3, Addr: "10.0.0.1:1080", State: 1, Clients: 2, IntervalBytes: 1234, AgeSeconds: 42}
	b, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var got proxyCellDetail
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Index != 3 || got.Addr != "10.0.0.1:1080" || got.IntervalBytes != 1234 || got.AgeSeconds != 42 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestProxyCellStoreConcurrentCellsAndDetail(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()
	for i := 0; i < 100; i++ {
		connect.RegisterProxy(i, "10.0.0.1:1080", "10.0.0.1:1080")
	}
	store := &proxyCellStore{prev: map[int]uint64{}}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.Cells(now)
		}()
		go func() {
			defer wg.Done()
			_ = store.Detail(now)
		}()
	}
	wg.Wait()
}

func TestProxyCellDetailAddressPrivacy(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	defer connect.ResetProxyHealthForTesting()
	connect.RegisterProxy(0, "198.51.100.1:1234", "direct")
	connect.RegisterProxy(1, "socks5://alice:secret@proxy.example:1080/path?token=hidden", "key")
	detail := (&proxyCellStore{prev: map[int]uint64{}}).Detail(time.Unix(100, 0))
	if len(detail) != 2 || detail[0].Addr != "direct (this host)" || detail[1].Addr != "proxy.example:1080" {
		t.Fatalf("unsafe detail: %+v", detail)
	}
}
