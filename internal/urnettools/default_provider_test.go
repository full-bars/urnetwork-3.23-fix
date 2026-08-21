package urnettools

import (
	"os"
	"testing"
)

func TestDefaultProviderRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := Target{Network: "mesocyclone", User: "urnet"}
	path, err := writeDefaultProvider(want)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readDefaultProvider()
	if got.Network != want.Network || got.User != want.User {
		t.Fatalf("read = %+v, want %+v", got, want)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("default file not at %s: %v", path, statErr)
	}
	if err := clearDefaultProvider(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := readDefaultProvider(); got.Unit != "" || got.Network != "" {
		t.Fatalf("expected empty default after clear, got %+v", got)
	}
}

func TestResolveDefaultProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	providers := []Provider{
		{User: "a", Network: "alpha", StateDir: "/srv/alpha"},
		{User: "b", Network: "mesocyclone", StateDir: "/srv/meso"},
	}
	// No default set -> ok=false.
	if _, ok := resolveDefaultProvider(providers); ok {
		t.Fatal("no default: expected ok=false")
	}
	// Default matching exactly one -> resolves.
	if _, err := writeDefaultProvider(Target{Network: "mesocyclone"}); err != nil {
		t.Fatal(err)
	}
	p, ok := resolveDefaultProvider(providers)
	if !ok || p.Network != "mesocyclone" {
		t.Fatalf("expected mesocyclone, got ok=%v p=%+v", ok, p)
	}
	// Stale default (matches nothing) -> ok=false, never acted on.
	if _, err := writeDefaultProvider(Target{Network: "ghost"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveDefaultProvider(providers); ok {
		t.Fatal("stale default: expected ok=false (never guess)")
	}
	// Ambiguous default (matches 2) -> ok=false, never acted on.
	if _, err := writeDefaultProvider(Target{User: "x", Network: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveDefaultProvider(providers); ok {
		t.Fatal("ambiguous default: expected ok=false")
	}
}
