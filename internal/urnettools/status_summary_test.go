package urnettools

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStatusSummaryRows(t *testing.T) {
	parked := 3
	risk := true
	p := Provider{Running: true, Version: "v1", Unit: "provider.service"}
	s := &NodeSnapshot{State: "degraded", RestartPending: true, Proxies: SnapshotProxies{Up: 10, Dead: 1, Parked: &parked, TrimReported: true, TrimCap: 12, Configured: 20}}
	rows := makeStatusRows(p, s, ExtenderReading{Known: true, Running: true}, &risk)
	if len(rows) != 6 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, i := range []int{1, 2, 4} {
		if rows[i].Level != statusWarn {
			t.Errorf("row %s: got %s", rows[i].Name, rows[i].Level)
		}
	}
	for _, want := range []string{"restart pending", "3 parked", "trim 12/20", "SIGPIPE risk"} {
		var b bytes.Buffer
		if err := renderStatusRows(&b, rows, 80, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %q", want, b.String())
		}
	}
}

func TestStatusSummaryUnknownAndStopped(t *testing.T) {
	rows := makeStatusRows(Provider{}, nil, ExtenderReading{}, nil)
	if rows[1].Level != statusBad || rows[2].Level != statusUnknown || rows[3].Level != statusUnknown || rows[4].Level != statusUnknown || rows[5].Level != statusUnknown {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestStatusSummaryNarrowAndNoColor(t *testing.T) {
	rows := makeStatusRows(Provider{Running: true, Version: "a very long version"}, nil, ExtenderReading{}, nil)
	var b bytes.Buffer
	if err := renderStatusRows(&b, rows, 12, false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if utf8.RuneCountInString(line) > 12 || strings.Contains(line, "\x1b") {
			t.Fatalf("bad line %q", line)
		}
	}
}
