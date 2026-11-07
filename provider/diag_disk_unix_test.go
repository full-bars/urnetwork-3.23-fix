//go:build unix

package main

import "testing"

func TestDiagAvailableBytes(t *testing.T) {
	if got := diagAvailableBytes(int64(-1), 4096); got != 0 {
		t.Errorf("negative available blocks: got %d bytes, want 0", got)
	}
	if got := diagAvailableBytes(int64(2), 4096); got != 8192 {
		t.Errorf("positive available blocks: got %d bytes, want 8192", got)
	}
	if got := diagAvailableBytes(uint64(1)<<63, 1); got != uint64(1)<<63 {
		t.Errorf("large unsigned available blocks: got %d", got)
	}
}
