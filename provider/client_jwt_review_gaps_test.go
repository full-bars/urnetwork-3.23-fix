package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJWTStoreNewMintTimeIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := newClientJWTStore(path)
	entry := clientJWTEntry{ByClientJWT: "same-token", MintedAt: time.Now().Add(-time.Hour)}
	if err := s.Put("proxy", entry); err != nil {
		t.Fatal(err)
	}
	entry.MintedAt = entry.MintedAt.Add(time.Minute)
	if err := s.Put("proxy", entry); err != nil {
		t.Fatal(err)
	}
	got, ok := newClientJWTStore(path).Get("proxy")
	if !ok || !got.MintedAt.Equal(entry.MintedAt) || s.flushes != 2 {
		t.Fatalf("mint time not persisted: got=%v ok=%v writes=%d", got.MintedAt, ok, s.flushes)
	}
}

func TestJWTStoreReviewCauseCases(t *testing.T) {
	now := time.Now()
	entry := clientJWTEntry{ByClientJWT: "token", MintedAt: now}
	stale := clientJWTEntry{ByClientJWT: "old", MintedAt: now.Add(-clientJWTStaleAfter - time.Hour)}
	for _, tc := range []struct {
		name    string
		disk    map[string]clientJWTEntry
		compact bool
		cause   string
	}{
		{"stale", map[string]clientJWTEntry{"x": entry, "old": stale}, false, "removed"},
		{"normalized", map[string]clientJWTEntry{"x": entry}, true, "normalized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			var b []byte
			var err error
			if tc.compact {
				b, err = json.Marshal(tc.disk)
			} else {
				b, err = json.MarshalIndent(tc.disk, "", "  ")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			s := newClientJWTStore(path)
			s.loaded = true
			out := captureTlog(t, func() {
				if err := s.flushBatchLocked(map[string]clientJWTEntry{"x": entry}, nil); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "cause="+tc.cause+" ") {
				t.Fatalf("want cause=%s: %s", tc.cause, out)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var entries map[string]clientJWTEntry
			if err := json.Unmarshal(got, &entries); err != nil {
				t.Fatal(err)
			}
			if !entries["x"].MintedAt.Equal(entry.MintedAt) {
				t.Fatal("minted_at changed")
			}
			if _, ok := entries["old"]; ok {
				t.Fatal("stale key survived")
			}
		})
	}
}
