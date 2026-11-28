package urnettools

import (
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

func TestNeonProxyRampRespectsColorDepth(t *testing.T) {
	for bucket := uint8(0); bucket < 8; bucket++ {
		got := neonProxyCellStyle(tui.NeonThemeDepth(tui.Depth16), connect.ProxyCellUp, bucket).FG
		if got.Kind != tui.Color16 {
			t.Errorf("16-color bucket %d: got %+v", bucket, got)
		}
	}
}
