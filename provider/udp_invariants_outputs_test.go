package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUDPInvariantCoverageAndMetricsOmitIdentity(t *testing.T) {
	const key = "192.0.2.66:24001\x1ffixture-account"
	store := newProxyUDPCheckStore(nil)
	store.entries[key] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: time.Now()}
	coverage := store.coverage([]string{key}, time.Now())
	if coverage.OK != 1 {
		t.Fatalf("coverage = %+v", coverage)
	}
	raw, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	var metrics strings.Builder
	writeUDPProxyMetrics(&metrics, coverage)
	for _, text := range []string{string(raw), metrics.String()} {
		if strings.Contains(text, "192.0.2.66") || strings.Contains(text, "24001") || strings.Contains(text, "fixture-account") {
			t.Fatal("coverage output contains proxy identity")
		}
	}
}
