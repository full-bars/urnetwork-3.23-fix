package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func TestTopUnitsKeyTogglesBetweenBinaryAndDecimal(t *testing.T) {
	t.Cleanup(func() { tui.SetDecimalUnits(false) })
	tui.SetDecimalUnits(false)
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	press := func(r rune) { m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: r}) }

	if tui.DecimalUnits() {
		t.Fatal("top starts in decimal units, want binary")
	}
	press('u')
	if !tui.DecimalUnits() {
		t.Fatal("u did not switch to decimal units")
	}
	press('U')
	if tui.DecimalUnits() {
		t.Fatal("U did not switch back to binary units")
	}
	found := false
	for _, line := range helpLines {
		found = found || strings.HasPrefix(line, "u ")
	}
	if !found {
		t.Fatal("the ? help does not list the u key")
	}
}

func TestTopBytesPairFollowsTheUnitChoice(t *testing.T) {
	t.Cleanup(func() { tui.SetDecimalUnits(false) })
	tui.SetDecimalUnits(false)
	if got := topBytesPair(1.8*1024*1024*1024, 4*1024*1024*1024); got != "1.8/4.0 GiB" {
		t.Fatalf("binary pair = %q", got)
	}
	if got := topBytesPair(512*1024*1024, 768*1024*1024); got != "512/768 MiB" {
		t.Fatalf("binary MiB pair = %q", got)
	}
	tui.SetDecimalUnits(true)
	if got := topBytesPair(1.8e9, 4e9); got != "1.8/4.0 GB" {
		t.Fatalf("decimal pair = %q", got)
	}
	if got := topBytesPair(512e6, 768e6); got != "512/768 MB" {
		t.Fatalf("decimal MB pair = %q", got)
	}
}
