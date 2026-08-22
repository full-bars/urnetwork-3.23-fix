package main

import "testing"

func TestProvenProxySet_TracksPerAddress(t *testing.T) {
	s := &provenProxySet{proven: map[string]bool{}}

	if s.HasSucceeded("1.2.3.4:1080") {
		t.Fatal("expected unmarked address to report not succeeded")
	}

	s.MarkSucceeded("1.2.3.4:1080")
	if !s.HasSucceeded("1.2.3.4:1080") {
		t.Fatal("expected marked address to report succeeded")
	}
	if s.HasSucceeded("5.6.7.8:1080") {
		t.Fatal("expected a different address to be unaffected")
	}
}
