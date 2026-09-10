package urnettools

import (
	"strings"
	"testing"
)

func TestConntrackMaxForRAMKB(t *testing.T) {
	tests := []struct {
		ramKB int
		want  int
	}{
		{512 * 1024, 131072},        // 512MB → 128K
		{1024 * 1024, 262144},       // 1GB → 256K
		{2 * 1024 * 1024, 262144},   // 2GB → 256K
		{4 * 1024 * 1024, 524288},   // 4GB → 512K
		{6 * 1024 * 1024, 524288},   // 6GB → 512K
		{8 * 1024 * 1024, 1048576},  // 8GB → 1M
		{12 * 1024 * 1024, 1048576}, // 12GB → 1M
		{16 * 1024 * 1024, 2097152}, // 16GB → 2M
		{24 * 1024 * 1024, 2097152}, // 24GB → 2M (honk)
		{32 * 1024 * 1024, 4194304}, // 32GB → 4M
		{64 * 1024 * 1024, 4194304}, // 64GB → 4M
	}
	for _, tt := range tests {
		got := conntrackMaxForRAMKB(tt.ramKB)
		ramGB := tt.ramKB / 1024 / 1024
		if got != tt.want {
			t.Errorf("conntrackMaxForRAMKB(%dGB) = %d, want %d", ramGB, got, tt.want)
		}
	}
}

func TestConntrackConfBlock(t *testing.T) {
	// With valid max: should produce conf block
	block := conntrackConfBlock(524288, "3600")
	if block == "" {
		t.Fatal("conntrackConfBlock(524288, 3600) returned empty")
	}
	for _, key := range []string{"nf_conntrack_max", "nf_conntrack_tcp_timeout_established"} {
		if !strings.Contains(block, key) {
			t.Errorf("conf block missing %q", key)
		}
	}

	// With zero max: should return empty
	block = conntrackConfBlock(0, "3600")
	if block != "" {
		t.Errorf("conntrackConfBlock(0, 3600) = %q, want empty", block)
	}
}

func TestConntrackMaxStr(t *testing.T) {
	if got := conntrackMaxStr(524288); got != "524288" {
		t.Errorf("conntrackMaxStr(524288) = %q, want %q", got, "524288")
	}
}
