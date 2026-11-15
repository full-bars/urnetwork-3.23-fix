package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

const (
	testKiB = 1024
	testMiB = 1024 * testKiB
	testGiB = 1024 * testMiB
	testTiB = 1024 * testGiB
)

// The traffic table prints every size in a human unit and sums the carriers.
func TestFormatExtenderTrafficHumanUnits(t *testing.T) {
	var b strings.Builder
	formatExtenderTraffic(&b, ExtenderReading{
		RelayBytesIn: 3*testGiB + 7, RelayBytesOut: 2 * testTiB,
		CarrierBytes: []ExtenderCarrierBytes{
			{Carrier: "tcp-443", IngressBytes: 1536, EgressBytes: 5 * testMiB, RateInBps: 2 * testKiB, RateOutBps: 3 * testMiB, LiveConns: 4, ConnsTotal: 90},
			{Carrier: "udp-443", IngressBytes: 3 * testGiB, EgressBytes: 2 * testTiB},
			{Carrier: "udp-53", IngressBytes: 7},
		}})
	out := b.String()
	for _, want := range []string{"Traffic", "carrier", "rate in", "connections",
		"tcp-443:", "1.5 KiB", "5.0 MiB", "2.0 KiB/s", "3.0 MiB/s", "4 live, 90 total",
		"udp-443:", "3.0 GiB", "2.0 TiB", "udp-53:", "7 B", "total:"} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic table is missing %q:\n%s", want, out)
		}
	}
	var none strings.Builder
	formatExtenderTraffic(&none, ExtenderReading{})
	if none.Len() != 0 {
		t.Errorf("no carriers must print nothing, got %q", none.String())
	}
}

// The Extender box of top shows one row per carrier that has carried traffic.
func TestTopExtenderBoxShowsCarrierTraffic(t *testing.T) {
	r := ExtenderReading{Source: SourceControlSocket, Running: true, RoleState: "active",
		CarrierBytes: []ExtenderCarrierBytes{
			{Carrier: "tcp-443", IngressBytes: 1200 * testMiB, EgressBytes: 4 * testGiB, LiveConns: 3},
			{Carrier: "udp-443", IngressBytes: 12 * testMiB, EgressBytes: 3 * testMiB},
			{Carrier: "udp-4053"},
		}}
	var rows []string
	for _, row := range extenderBoxRowsFor(r, topBase, false) {
		rows = append(rows, row.key+" "+row.value)
	}
	got := strings.Join(rows, "\n")
	for _, want := range []string{"tcp443 ↓1.2 GiB ↑4.0 GiB", "udp443 ↓12.0 MiB ↑3.0 MiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("rows are missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "udp4053") {
		t.Errorf("a carrier with no traffic must not take a row:\n%s", got)
	}
	var ascii []string
	for _, row := range extenderBoxRowsFor(r, topBase, true) {
		ascii = append(ascii, row.value)
	}
	if joined := strings.Join(ascii, "\n"); !strings.Contains(joined, "v1.2 GiB ^4.0 GiB") || strings.ContainsAny(joined, "↓↑") {
		t.Errorf("an ASCII theme must not get arrows:\n%s", joined)
	}
	// the box height follows the rows, so the traffic never pushes a row out of the frame
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.applyExtender(m.gen, r)
	if want := len(extenderBoxRowsFor(r, m.now(), false)) + 2; m.extenderRows() != want {
		t.Errorf("extenderRows = %d, want %d", m.extenderRows(), want)
	}
}
