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

func TestFormatExtenderTrafficFamilySplit(t *testing.T) {
	var b strings.Builder
	formatExtenderTraffic(&b, ExtenderReading{
		RelayBytesIn: 2 * testGiB, RelayBytesOut: 4 * testGiB,
		CarrierBytes: []ExtenderCarrierBytes{
			{
				Carrier:        "tcp-443",
				IngressBytes:   2 * testGiB,
				EgressBytes:    4 * testGiB,
				RateInBps:      10 * testMiB,
				RateOutBps:     20 * testMiB,
				LiveConns:      6,
				ConnsTotal:     100,
				IngressBytesV4: 1500 * testMiB,
				EgressBytesV4:  3 * testGiB,
				LiveConnsV4:    4,
				ConnsTotalV4:   70,
				IngressBytesV6: 500 * testMiB,
				EgressBytesV6:  1 * testGiB,
				LiveConnsV6:    2,
				ConnsTotalV6:   30,
			},
		},
	})
	out := b.String()
	for _, want := range []string{
		"tcp-443:", "2.0 GiB", "4.0 GiB", "6 live, 100 total",
		"ipv4:", "1.5 GiB", "3.0 GiB", "4 live, 70 total",
		"ipv6:", "500.0 MiB", "1.0 GiB", "2 live, 30 total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic family table missing %q:\n%s", want, out)
		}
	}
}

func TestTopExtenderBoxShowsFamilySplit(t *testing.T) {
	r := ExtenderReading{
		Source:    SourceControlSocket,
		Running:   true,
		RoleState: "active",
		CarrierBytes: []ExtenderCarrierBytes{
			{
				Carrier:        "tcp-443",
				IngressBytes:   1200 * testMiB,
				EgressBytes:    4 * testGiB,
				LiveConns:      6,
				IngressBytesV4: 1000 * testMiB,
				EgressBytesV4:  3 * testGiB,
				LiveConnsV4:    4,
				IngressBytesV6: 200 * testMiB,
				EgressBytesV6:  1 * testGiB,
				LiveConnsV6:    2,
			},
		},
	}
	var rows []string
	for _, row := range extenderBoxRowsFor(r, topBase, false) {
		rows = append(rows, row.key+" "+row.value)
	}
	got := strings.Join(rows, "\n")
	for _, want := range []string{
		"tcp443 ↓1.2 GiB ↑4.0 GiB",
		"  ipv4 ↓1000.0 MiB ↑3.0 GiB",
		"  ipv6 ↓200.0 MiB ↑1.0 GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rows missing %q:\n%s", want, got)
		}
	}

	// ASCII theme
	var asciiRows []string
	for _, row := range extenderBoxRowsFor(r, topBase, true) {
		asciiRows = append(asciiRows, row.key+" "+row.value)
	}
	asciiGot := strings.Join(asciiRows, "\n")
	for _, want := range []string{
		"tcp443 v1.2 GiB ^4.0 GiB",
		"  ipv4 v1000.0 MiB ^3.0 GiB",
		"  ipv6 v200.0 MiB ^1.0 GiB",
	} {
		if !strings.Contains(asciiGot, want) {
			t.Errorf("ascii rows missing %q:\n%s", want, asciiGot)
		}
	}
	if strings.ContainsAny(asciiGot, "↓↑") {
		t.Errorf("ascii rows contain Unicode arrows:\n%s", asciiGot)
	}
}

func TestTopExtenderBoxTerminalWidthsAndThemes(t *testing.T) {
	r := ExtenderReading{
		Source:    SourceControlSocket,
		Running:   true,
		RoleState: "active",
		Ports: []ExtenderPort{
			{Name: "tcp 443", Up: true},
			{Name: "udp 443", Up: true},
		},
		Families: []ExtenderFamily{
			{IpVersion: 4, State: "activated"},
			{IpVersion: 6, State: "activated"},
		},
		CarrierBytes: []ExtenderCarrierBytes{
			{
				Carrier:        "tcp-443",
				IngressBytes:   1200 * testMiB,
				EgressBytes:    4 * testGiB,
				LiveConns:      6,
				IngressBytesV4: 1000 * testMiB,
				EgressBytesV4:  3 * testGiB,
				LiveConnsV4:    4,
				IngressBytesV6: 200 * testMiB,
				EgressBytesV6:  1 * testGiB,
				LiveConnsV6:    2,
			},
			{
				Carrier:        "udp-443",
				IngressBytes:   50 * testMiB,
				EgressBytes:    100 * testMiB,
				LiveConns:      2,
				IngressBytesV4: 30 * testMiB,
				EgressBytesV4:  60 * testMiB,
				LiveConnsV4:    1,
				IngressBytesV6: 20 * testMiB,
				EgressBytesV6:  40 * testMiB,
				LiveConnsV6:    1,
			},
		},
	}

	clock := &fakeClock{t: topBase}
	cbTheme, _ := tui.ThemeByName("colorblind")
	themes := []tui.Theme{
		tui.DefaultTheme(),
		tui.MonoTheme(),
		cbTheme,
	}

	widths := []int{80, 120, 160}
	for _, th := range themes {
		for _, w := range widths {
			m := newTestModel(th, topProviders(1), clock)
			m.applyExtender(m.gen, r)

			// render into a buffer matching side column width
			sideW := m.sideCols(w)
			b := tui.New(sideW, m.extenderRows())
			m.drawExtender(b)

			rendered := b.String()
			if !strings.Contains(rendered, "tcp443") {
				t.Errorf("theme %s width %d missing tcp443 in rendered buffer:\n%s", th.Name, w, rendered)
			}
			if !strings.Contains(rendered, "ipv4") || !strings.Contains(rendered, "ipv6") {
				t.Errorf("theme %s width %d missing family breakdown in rendered buffer:\n%s", th.Name, w, rendered)
			}
			if th.ASCII && strings.ContainsAny(rendered, "↓↑") {
				t.Errorf("theme %s width %d has unicode arrows in ASCII mode:\n%s", th.Name, w, rendered)
			}
		}
	}
}
