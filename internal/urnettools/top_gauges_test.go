package urnettools

import (
	"github.com/urnetwork/connect/internal/tui"
	"strings"
	"testing"
)

func TestTopGaugesUnknownLimitAndNarrow(t *testing.T) {
	m := newTopModel(nil, 0, 0, tui.Theme{}, nil)
	b := tui.New(80, 4)
	m.drawGauge(b, 0, "gor", "123", 123, 0)
	var line strings.Builder
	for x := 0; x < b.Width(); x++ {
		line.WriteRune(b.Cell(x, 0).Rune)
	}
	if !strings.Contains(line.String(), "123") || strings.Contains(line.String(), "/") {
		t.Fatalf("unknown limit: %s", line.String())
	}
	m.drawGauge(tui.New(1, 1), 0, "fds", "80/100", 80, 100)
}
