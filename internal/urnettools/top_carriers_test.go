package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

func TestTopCarriersRing(t *testing.T) {
	base := time.Date(2026, time.November, 15, 0, 0, 0, 0, time.UTC)
	var h topCarrierHistory
	h.push(base, "t1", 5)
	h.push(base.Add(30*time.Second), "t1", 9) // not a minute yet, ignored
	if r := h.per["t1"]; r == nil || len(r.ring) != 0 {
		t.Fatalf("half a minute is not a minute: %+v", r)
	}
	h.push(base.Add(time.Minute), "t1", 25)
	h.push(base.Add(2*time.Minute), "t1", 20) // counter dropped: re-base silently
	r := h.per["t1"]
	if len(r.ring) != 1 || r.ring[0] != 20 {
		t.Fatalf("minute delta: %+v", r.ring)
	}
}

func TestTopCarriersRowList(t *testing.T) {
	rows := carrierRows(ExtenderReading{})
	if len(rows) != 0 {
		t.Fatalf("unknown reading must not list carriers: %+v", rows)
	}
	r := ExtenderReading{Source: SourceControlSocket, Known: true, Carriers: []ExtenderCarrierStats{{Carrier: "t1", Served: 5, Refused: 2, RefusalsByStage: map[string]uint64{"auth": 1}}}, Families: []ExtenderFamily{{IpVersion: 4, State: "activated"}, {IpVersion: 6, State: "held"}}}
	rows = carrierRows(r)
	if len(rows) != 1 || rows[0].name != "t1" || rows[0].served != 5 || rows[0].refused != 2 || !rows[0].ipv4 || rows[0].ipv6 {
		t.Fatalf("row = %+v", rows)
	}
	if got := strings.Join(rows[0].reasons, ","); got != "auth 1" {
		t.Fatalf("reasons = %q", got)
	}
	m := newTopModel(nil, 0, 0, tui.Theme{}, nil)
	b := tui.New(80, 4)
	m.drawCarriers(b)
	if got := b.String(); !strings.Contains(got, "no data") {
		t.Fatalf("unknown reading must render a placeholder:\n%s", got)
	}
	m.extender = r
	m.applyExtender(m.gen, r)
	b = tui.New(80, 4)
	m.drawCarriers(b)
	if got := b.String(); !strings.Contains(got, "t1") || !strings.Contains(got, "refused") {
		t.Fatalf("render:\n%s", got)
	}
}
