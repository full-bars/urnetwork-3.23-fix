package urnettools

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestExtenderStatusCarrierVisibility(t *testing.T) {
	r := ExtenderReading{Source: SourceControlSocket, Known: true, Running: true,
		Ports: []ExtenderPort{{Name: "tcp 443", Up: true}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{{IpVersion: 4, State: "activated", Ports: []ExtenderFamilyPort{
			{Name: "tcp 443", State: "bound, accepted by the platform"},
			{Name: "udp 443", State: "bound, left out by the platform (probe failed)"},
			{Name: "dns 4053", State: "bound, left out by the platform (probe failed)"},
		}}},
	}
	var b bytes.Buffer
	formatExtenderBlock(&b, r, time.Time{})
	for _, want := range []string{"tcp 443: bound, accepted", "udp 443: bound, left out", "dns 4053: bound, left out"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
	rows := extenderBoxRows(r, time.Time{})
	count := 0
	for _, row := range rows {
		if strings.Contains(row.value, "left out") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("top left-out rows = %d: %+v", count, rows)
	}
}
