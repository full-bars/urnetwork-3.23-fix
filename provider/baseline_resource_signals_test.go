package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseSockstat(t *testing.T) {
	content := "sockets: used 190\nTCP: inuse 29 orphan 3 tw 112 alloc 40 mem 7\nUDP: inuse 4 mem 2\n"
	tw, orphans, ok := parseSockstat(content)
	if !ok || tw != 112 || orphans != 3 {
		t.Fatalf("parseSockstat = (%d, %d, %v), want (112, 3, true)", tw, orphans, ok)
	}
}

func TestParseSockstatMissingTCPLine(t *testing.T) {
	if _, _, ok := parseSockstat("sockets: used 190\nUDP: inuse 4 mem 2\n"); ok {
		t.Fatal("ok = true with no TCP line, want false")
	}
	if _, _, ok := parseSockstat("TCP: inuse 29 alloc 40 mem 7\n"); ok {
		t.Fatal("ok = true with neither tw nor orphan, want false")
	}
}

func TestConntrackUsedFrac(t *testing.T) {
	if f, ok := conntrackUsedFrac(131072, 262144); !ok || !almostEq(f, 0.5) {
		t.Fatalf("conntrackUsedFrac(131072, 262144) = (%v, %v), want (0.5, true)", f, ok)
	}
	if _, ok := conntrackUsedFrac(10, 0); ok {
		t.Fatal("a zero max must read as unknown, not a ratio")
	}
	// A real zero count is data, not "unknown".
	if f, ok := conntrackUsedFrac(0, 1000); !ok || f != 0 {
		t.Fatalf("conntrackUsedFrac(0, 1000) = (%v, %v), want (0, true)", f, ok)
	}
}

func TestBaselineSampleCarriesResourceSignals(t *testing.T) {
	frac, tw, orph := 0.42, int64(112), int64(3)
	cpu := 0.07
	in := baselineInputs{
		now:  time.Unix(1_700_000_000, 0),
		snap: &NodeSnapshot{Version: "v1"},
		gc:   &baselineGC{GOGC: 50, Tightening: true, CPUFrac: &cpu},
		net:  &baselineNet{ConntrackUsedFrac: &frac, TimeWait: &tw, Orphans: &orph},
	}
	b, err := json.Marshal(buildBaselineSample(in))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		GC  *baselineGC  `json:"gc"`
		Net *baselineNet `json:"net"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.GC == nil || got.GC.GOGC != 50 || !got.GC.Tightening || got.GC.CPUFrac == nil || !almostEq(*got.GC.CPUFrac, 0.07) {
		t.Fatalf("gc = %+v", got.GC)
	}
	if got.Net == nil || got.Net.ConntrackUsedFrac == nil || *got.Net.TimeWait != 112 || *got.Net.Orphans != 3 {
		t.Fatalf("net = %+v", got.Net)
	}
}

// Unknown must stay distinguishable from zero on disk.
func TestBaselineSampleOmitsUnknownResourceSignals(t *testing.T) {
	b, err := json.Marshal(buildBaselineSample(baselineInputs{now: time.Unix(1_700_000_000, 0), snap: &NodeSnapshot{Version: "v1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"gc"`) || strings.Contains(string(b), `"net"`) {
		t.Fatalf("sample carries gc/net blocks although nothing was read: %s", b)
	}
}

func TestResourceSignalMetricsOmitUnknownSeries(t *testing.T) {
	frac := 0.25
	var sb strings.Builder
	writeResourceSignalGauges(&sb, &baselineGC{GOGC: 100}, &baselineNet{ConntrackUsedFrac: &frac})
	out := sb.String()
	for _, want := range []string{
		"# TYPE urnet_gc_gogc gauge", "urnet_gc_gogc 100",
		"# TYPE urnet_gc_tightening gauge", "urnet_gc_tightening 0",
		"# TYPE urnet_conntrack_used_ratio gauge", "urnet_conntrack_used_ratio 0.25",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, absent := range []string{"urnet_gc_cpu_fraction", "urnet_tcp_time_wait", "urnet_tcp_orphans"} {
		if strings.Contains(out, absent) {
			t.Errorf("unknown series %s must be omitted, got:\n%s", absent, out)
		}
	}
	var none strings.Builder
	writeResourceSignalGauges(&none, nil, nil)
	if none.Len() != 0 {
		t.Fatalf("nil inputs wrote %q, want nothing", none.String())
	}
}
