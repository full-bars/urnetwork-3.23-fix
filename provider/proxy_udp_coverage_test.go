package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestProxyUDPCoverage(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"ok":              {result: proxyUDPResult{Status: proxyUDPOK}, at: now},
		"no":              {result: proxyUDPResult{Status: proxyUDPNoRelay}, at: now},
		"mismatch":        {result: proxyUDPResult{Status: proxyUDPExitMismatch}, at: now},
		"unknown":         {result: proxyUDPResult{Status: proxyUDPUnknown}, at: now},
		"expired":         {result: proxyUDPResult{Status: proxyUDPOK}, at: now.Add(-proxyUDPOKTTL)},
		"unknown-expired": {result: proxyUDPResult{Status: proxyUDPUnknown}, at: now.Add(-proxyUDPUnknownTTL)},
		"running":         {done: make(chan struct{})},
		"offline":         {result: proxyUDPResult{Status: proxyUDPOK}, at: now},
	}
	got := store.coverage([]string{"ok", "no", "mismatch", "unknown", "expired", "unknown-expired", "running", "missing"}, now)
	want := &SnapshotProxyUDP{OK: 1, NoRelay: 1, Mismatch: 1, Unknown: 1, Unchecked: 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSnapshotProxyUDPJSON(t *testing.T) {
	original := SnapshotProxies{Up: 2, UDP: &SnapshotProxyUDP{OK: 1, Unchecked: 1}}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SnapshotProxies
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip: %+v", decoded)
	}
	if err := json.Unmarshal([]byte(`{"up":2,"degraded":0,"connecting":0,"dead":0}`), &decoded); err != nil {
		t.Fatal(err)
	}
	// Decode into a fresh value as readers do for each reply.
	decoded = SnapshotProxies{}
	if err := json.Unmarshal([]byte(`{"up":2}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.UDP != nil {
		t.Fatal("old snapshot should not report UDP")
	}
}
