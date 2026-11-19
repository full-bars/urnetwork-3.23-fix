package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUDPPaidDesiredFileExcludesInternal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	if err := os.Mkdir(filepath.Join(dir, ".urnetwork"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".urnetwork", "proxy"), []byte(`{"servers":{"192.0.2.2:1080":""}}`), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("192.0.2.1:1080:user:pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, trusted := udpPaidDesiredSet(&ProxyState{Source: source})
	if !trusted || len(got) != 1 {
		t.Fatalf("trusted=%v entries=%d", trusted, len(got))
	}
	for _, settings := range got {
		if settings.Address != "192.0.2.1:1080" {
			t.Fatal("unexpected source")
		}
	}
	internal, trusted := udpPaidDesiredSet(&ProxyState{})
	if !trusted || len(internal) != 1 {
		t.Fatalf("internal trusted=%v entries=%d", trusted, len(internal))
	}
	for _, settings := range internal {
		if settings.Address != "192.0.2.2:1080" {
			t.Fatal("unexpected internal entry")
		}
	}
}
