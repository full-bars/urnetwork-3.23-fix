package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestJWTStoreAttribution(t *testing.T) {
	s := newClientJWTStore(filepath.Join(t.TempDir(), "store"))
	e := clientJWTEntry{MintedAt: time.Now()}
	for _, tc := range []struct {
		name string
		op   string
		run  func() error
	}{
		{"save", "put", func() error { return s.Put("x", e) }},
		{"skip", "put", func() error { return s.Put("x", e) }},
		{"delete", "delete", func() error { return s.Delete("x") }},
		{"batch", "batch", func() error { return s.flushBatchLocked(map[string]clientJWTEntry{"x": e}, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureTlog(t, func() {
				if err := tc.run(); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "site=unknown op="+tc.op+" ") {
				t.Fatalf("missing bounded attribution: %s", out)
			}
		})
	}
}

func TestJWTStoreAdoptionAttribution(t *testing.T) {
	s := newClientJWTStore(filepath.Join(t.TempDir(), "store"))
	if err := s.Put("example:1080", clientJWTEntry{MintedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out := captureTlog(t, func() {
		s.AdoptLegacy([]*connect.ProxySettings{credSettings("example:1080", "user")})
	})
	if !strings.Contains(out, "site=client_jwt_store.go:") || !strings.Contains(out, " op=batch ") {
		t.Fatalf("missing adoption call site: %s", out)
	}
}

func TestJWTStoreSiteAllowlist(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/home/private/project/provider/main.go", "main.go:123"},
		{"provider/renewal_watcher.go", "renewal_watcher.go:123"},
		{"provider/client_jwt_store.go", "client_jwt_store.go:123"},
		{"/home/private/main.go.secret", "unknown"},
		{"/home/private/other.go", "unknown"},
	} {
		if got := jwtStoreSite(tc.path, 123); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.path, got, tc.want)
		}
	}
}
