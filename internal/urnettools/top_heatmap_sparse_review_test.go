package urnettools

import (
	"strings"
	"testing"
)

func TestProxyCellInfoSparseIndices(t *testing.T) {
	m := &topModel{proxyCells: []proxyCell{{index: 7}, {index: 2}}}
	if got := m.proxyCellInfo(7); len(got) == 0 || !strings.Contains(got[0], "proxy[7]") {
		t.Fatalf("index 7: %v", got)
	}
	if got := m.proxyCellInfo(0); got != nil {
		t.Fatalf("missing index 0: %v", got)
	}
}
