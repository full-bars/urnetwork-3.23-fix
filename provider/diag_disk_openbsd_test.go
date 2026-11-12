package main

import "testing"

func TestOpenBSDDiskFreeBytes(t *testing.T) {
	if _, err := diskFreeBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := diskFreeBytes("/nonexistent/urnetwork-openbsd-test"); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}
