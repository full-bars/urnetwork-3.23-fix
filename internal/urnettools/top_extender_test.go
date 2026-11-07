package urnettools

import (
	"strings"
	"testing"
	"time"
)

func extenderRowMap(rows []topExtenderRow) map[string]topExtenderRow {
	m := map[string]topExtenderRow{}
	for _, r := range rows {
		m[r.key] = r
	}
	return m
}

func TestTopExtenderRowsServing(t *testing.T) {
	now := time.Now()
	rows := extenderRowMap(extenderBoxRows(ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", Running: true, RoleState: "serving", Activating: true,
		Ports: []ExtenderPort{{Name: "tcp 443", Up: true}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{
			{IpVersion: 4, State: "activated", Ip: "203.0.113.5"},
			{IpVersion: 6, State: "failed", Detail: "no public IPv6 address"},
		},
		LiveClients: 3,
		Served:      ExtenderServed{Last24h: 120, Last7d: 800, Lifetime: 5000},
	}, now))
	if r := rows["state"]; r.value != "serving" || r.level != topGood {
		t.Errorf("state row = %+v", r)
	}
	if r := rows["ipv4"]; !strings.Contains(r.value, "ACTIVE 203.0.113.5") || r.level != topGood {
		t.Errorf("ipv4 row = %+v", r)
	}
	if r := rows["ipv6"]; !strings.Contains(r.value, "not active") || r.level != topWarn {
		t.Errorf("ipv6 row = %+v", r)
	}
	if r := rows["clients"]; r.value != "3 now" {
		t.Errorf("clients row = %+v", r)
	}
	if r := rows["served"]; !strings.Contains(r.value, "120") || !strings.Contains(r.value, "5000") {
		t.Errorf("served row = %+v", r)
	}
}

func TestTopExtenderRowsBlockedTcpIsBad(t *testing.T) {
	rows := extenderRowMap(extenderBoxRows(ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", Running: true, RoleState: "tcp_unavailable",
		Ports:    []ExtenderPort{{Name: "tcp 443", Up: false, Error: "address already in use"}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{{IpVersion: 4, State: "held"}, {IpVersion: 6, State: "held"}},
	}, time.Now()))
	if r := rows["state"]; r.level != topBad || !strings.Contains(r.value, "tcp 443") {
		t.Errorf("a blocked tcp 443 must read bad and say why: %+v", r)
	}
	if r := rows["ports"]; !strings.Contains(r.value, "tcp DOWN") || r.level != topBad {
		t.Errorf("ports row = %+v", r)
	}
}

func TestTopExtenderRowsOffAndUnavailable(t *testing.T) {
	off := extenderRowMap(extenderBoxRows(ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "off", RoleState: "off",
		OffReason: "turned off by control socket. Turn it back on with: urnet-tools extender on",
	}, time.Now()))
	if r := off["state"]; r.value != "OFF" || r.level != topWarn {
		t.Errorf("off state row = %+v", r)
	}
	if r := off["turn on"]; !strings.Contains(r.value, "urnet-tools extender on") {
		t.Errorf("off must say how to turn it on: %+v", r)
	}
	if rows := extenderBoxRows(ExtenderReading{Source: SourceUnknown}, time.Now()); len(rows) != 0 {
		t.Errorf("a provider that cannot answer gets no box (older builds), got %+v", rows)
	}
}

// The box is fetched off the UI loop and throttled, and a reading fetched for an
// earlier provider is dropped.
func TestTopModelExtenderFetchIsThrottledAndGenerationChecked(t *testing.T) {
	m := &topModel{providers: []Provider{{StateDir: "/x"}}, interval: time.Second}
	now := time.Now()
	gen, _, ok := m.wantExtender(now)
	if !ok {
		t.Fatal("first extender fetch must be wanted")
	}
	if _, _, ok := m.wantExtender(now.Add(time.Millisecond)); ok {
		t.Fatal("a second fetch while one is in flight must not be started")
	}
	m.applyExtender(gen+1, ExtenderReading{Source: SourceControlSocket, Known: true})
	if m.extender.Source == SourceControlSocket {
		t.Fatal("a reading for another generation was applied")
	}
	m.applyExtender(gen, ExtenderReading{Source: SourceControlSocket, Known: true})
	if m.extender.Source != SourceControlSocket || m.extenderBusy {
		t.Fatal("the current reading was not applied or the busy flag stayed set")
	}
	if _, _, ok := m.wantExtender(now.Add(10 * time.Millisecond)); ok {
		t.Fatal("a fetch right after one finished must wait for the interval")
	}
	if _, _, ok := m.wantExtender(now.Add(5 * time.Second)); !ok {
		t.Fatal("a fetch after the interval must be wanted again")
	}
}

func TestTopModelExtenderStateResetOnSelectProvider(t *testing.T) {
	m := &topModel{
		providers: []Provider{{StateDir: "/a"}, {StateDir: "/b"}},
		interval:  time.Second,
	}
	now := time.Now()
	gen, _, ok := m.wantExtender(now)
	if !ok {
		t.Fatal("expected first extender fetch to be wanted")
	}
	if !m.extenderBusy {
		t.Fatal("expected extenderBusy to be true while fetch is in flight")
	}

	m.extender = ExtenderReading{Source: SourceControlSocket, Known: true}
	m.selectProvider(1)
	if m.extenderBusy || m.extender.Known || !m.lastExtender.IsZero() {
		t.Fatal("provider switch did not reset extender state")
	}
	newGen, _, ok := m.wantExtender(now)
	if !ok {
		t.Fatal("expected a fetch for the new provider")
	}
	m.applyExtender(gen, ExtenderReading{Source: SourceControlSocket, Known: true})
	if !m.extenderBusy || m.extender.Known {
		t.Fatal("stale response changed the new provider's in-flight fetch")
	}
	m.applyExtender(newGen, ExtenderReading{Source: SourceControlSocket, Known: true})
	if m.extenderBusy || !m.extender.Known {
		t.Fatal("new provider's response was not applied")
	}
}

// The box is on screen, under Resources, with the numbers an operator reads.
func TestTopRendersTheExtenderBox(t *testing.T) {
	m := modelWithProxies(t, 1, 0, 0, 0)
	m.extender = ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", Running: true, RoleState: "serving", Activating: true,
		Ports: []ExtenderPort{{Name: "tcp 443", Up: true}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{
			{IpVersion: 4, State: "activated", Ip: "203.0.113.5"},
			{IpVersion: 6, State: "failed", Detail: "no public IPv6"},
		},
		LiveClients: 3,
		Served:      ExtenderServed{Last24h: 120, Last7d: 800, Lifetime: 5000},
	}
	txt, _ := showOnSim(t, m, 120, 50)
	for _, want := range []string{"Extender", "serving", "ACTIVE 203.0.113.5", "not active", "3 now", "120 24h"} {
		if !strings.Contains(txt, want) {
			t.Errorf("screen is missing %q:\n%s", want, txt)
		}
	}
}

// A provider that cannot report the extender gets no box at all.
func TestTopLeavesOutTheExtenderBoxWhenUnreported(t *testing.T) {
	m := modelWithProxies(t, 1, 0, 0, 0)
	m.extender = ExtenderReading{Source: SourceUnknown}
	txt, _ := showOnSim(t, m, 120, 50)
	if strings.Contains(txt, "Extender") {
		t.Errorf("an unreported extender must not draw a box:\n%s", txt)
	}
}
