package main

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

func TestExtenderHealthLineWhenOff(t *testing.T) {
	line := formatExtenderHealthLine(extenderControlReading{
		Known:     true,
		Setting:   "off",
		OffReason: "turned off by control socket (urnet-tools extender off). Turn it back on with: urnet-tools extender on",
	}, time.Now())
	for _, want := range []string{"[health][extender]", "OFF", "urnet-tools extender on"} {
		if !strings.Contains(line, want) {
			t.Errorf("off line %q is missing %q", line, want)
		}
	}
}

// One line, readable at 3am: is it on, which ports are up, what each address
// family is doing and when it tries next, and who it is helping.
func TestExtenderHealthLineServing(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	next := now.Add(23 * time.Minute)
	line := formatExtenderHealthLine(extenderControlReading{
		Known: true, Setting: "on", Running: true, RoleState: extenderStateServing,
		Ports: []extenderPortReading{
			{Name: "tcp 443", Up: true},
			{Name: "udp 443", Up: true},
			{Name: "dns 4053", Up: true},
		},
		Families: []extenderFamilyReading{
			{IpVersion: 4, State: extenderFamilyActivated, Ip: "203.0.113.5"},
			{IpVersion: 6, State: extenderFamilyFailed, Detail: "no public IPv6 address", NextAttemptUnixMs: next.UnixMilli()},
		},
		LiveClients: 2,
		Served:      extenderTallyWindows{Last24h: 120, Last7d: 800, Lifetime: 5000},
	}, now)
	for _, want := range []string{
		"serving",
		"tcp 443 bound", "udp 443 bound", "dns 4053 bound",
		"IPv4 ACTIVE 203.0.113.5",
		"IPv6 not active", "no public IPv6 address", "next try",
		"clients now 2", "24h 120", "7d 800", "lifetime 5000",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("serving line is missing %q:\n%s", want, line)
		}
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("the health line must be one line, got:\n%s", line)
	}
}

// A blocked tcp 443 keeps the host from being listed. The line must say so in
// words, and not leave the operator to infer it from a missing "up".
func TestExtenderHealthLineNamesBlockedTcp(t *testing.T) {
	line := formatExtenderHealthLine(extenderControlReading{
		Known: true, Setting: "on", Running: true, RoleState: extenderStateTcpUnavailable,
		Ports: []extenderPortReading{
			{Name: "tcp 443", Up: false, Error: "address already in use"},
			{Name: "udp 443", Up: true},
			{Name: "dns 4053", Up: true},
		},
		Families: []extenderFamilyReading{
			{IpVersion: 4, State: extenderFamilyHeld},
			{IpVersion: 6, State: extenderFamilyHeld},
		},
	}, time.Now())
	for _, want := range []string{"tcp 443 DOWN", "address already in use", "NOT listed"} {
		if !strings.Contains(line, want) {
			t.Errorf("blocked line is missing %q:\n%s", want, line)
		}
	}
}

func TestExtenderHealthLineBeforeTheDirectSlotAttaches(t *testing.T) {
	line := formatExtenderHealthLine(extenderControlReading{
		Known: true, Setting: "on", Running: true, RoleState: extenderStateServing,
		Ports: []extenderPortReading{{Name: "tcp 443", Up: true}},
		Families: []extenderFamilyReading{
			{IpVersion: 4, State: extenderFamilyWaiting},
			{IpVersion: 6, State: extenderFamilyWaiting},
		},
	}, time.Now())
	if !strings.Contains(line, "waiting") {
		t.Errorf("a family with no outcome yet should read as waiting:\n%s", line)
	}
}

// A listing the platform signed has an expiry. After it passes, a family that was
// never re-activated is not ACTIVE, whatever the stale flag says.
func TestExtenderFamilyReadingNeverShowsAnExpiredListingAsActive(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	active := func(expire time.Time) *connect.ExtenderFamilyActivationStatus {
		return &connect.ExtenderFamilyActivationStatus{
			IpVersion: 4, Activated: true, Ip: netip.MustParseAddr("203.0.113.5"), ExpireTime: expire,
		}
	}
	fresh := extenderFamilyReadingOf(4, active(now.Add(time.Hour)), true, true, now)
	if fresh.State != extenderFamilyActivated {
		t.Fatalf("a valid listing must read activated, got %q", fresh.State)
	}
	stale := extenderFamilyReadingOf(4, active(now.Add(-time.Minute)), true, true, now)
	if stale.State == extenderFamilyActivated {
		t.Fatal("an expired listing still reads ACTIVE")
	}
	if !strings.Contains(stale.Detail, "expired") {
		t.Fatalf("the reason must say the listing expired, got %q", stale.Detail)
	}
	// no expiry reported at all (older activator): trust the flag, as before
	if got := extenderFamilyReadingOf(4, active(time.Time{}), true, true, now); got.State != extenderFamilyActivated {
		t.Fatalf("a listing with no reported expiry must keep reading activated, got %q", got.State)
	}
}

// The status carries the expiry in two fields (the activation's and the record's).
// Either one past means the listing lapsed, so a family with only the record's
// expiry reported is held to it as well.
func TestExtenderFamilyReadingUsesTheRecordExpiryWhenTheActivationHasNone(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	family := &connect.ExtenderFamilyActivationStatus{
		IpVersion: 4, Activated: true, Ip: netip.MustParseAddr("203.0.113.5"),
		RecordExpireTime: now.Add(-time.Minute),
	}
	if got := extenderFamilyReadingOf(4, family, true, true, now); got.State == extenderFamilyActivated {
		t.Fatalf("a record that expired a minute ago still reads ACTIVE: %+v", got)
	}
	family.RecordExpireTime = now.Add(time.Hour)
	if got := extenderFamilyReadingOf(4, family, true, true, now); got.State != extenderFamilyActivated {
		t.Fatalf("a record that is still valid must read activated, got %q", got.State)
	}
}

func TestExtenderCarrierBytesReadingBackwardCompatibility(t *testing.T) {
	// JSON payload from older provider without family fields
	oldJSON := `{"carrier":"tcp-443","ingress_bytes":1000,"egress_bytes":2000,"live_conns":5,"conns_total":20}`
	var r extenderCarrierBytesReading
	if err := json.Unmarshal([]byte(oldJSON), &r); err != nil {
		t.Fatalf("unmarshal old JSON: %v", err)
	}
	if r.Carrier != "tcp-443" || r.IngressBytes != 1000 || r.EgressBytes != 2000 || r.LiveConns != 5 || r.ConnsTotal != 20 {
		t.Fatalf("unexpected unmarshaled totals: %+v", r)
	}
	if r.IngressBytesV4 != 0 || r.IngressBytesV6 != 0 || r.EgressBytesV4 != 0 || r.EgressBytesV6 != 0 {
		t.Fatalf("family fields must default to 0, got: %+v", r)
	}

	// Marshaling with family fields
	r.IngressBytesV4 = 700
	r.IngressBytesV6 = 300
	r.EgressBytesV4 = 1400
	r.EgressBytesV6 = 600
	r.LiveConnsV4 = 3
	r.LiveConnsV6 = 2
	r.ConnsTotalV4 = 14
	r.ConnsTotalV6 = 6

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"ingress_bytes_v4":700`,
		`"ingress_bytes_v6":300`,
		`"egress_bytes_v4":1400`,
		`"egress_bytes_v6":600`,
		`"live_conns_v4":3`,
		`"live_conns_v6":2`,
		`"conns_total_v4":14`,
		`"conns_total_v6":6`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("marshaled json missing %s:\n%s", want, string(data))
		}
	}
}

func TestExtenderMetricsCarrierFamilySplit(t *testing.T) {
	snapshot := extenderRoleSnapshot{
		Enabled: true,
		State:   extenderStateServing,
		CarrierBytes: map[string]extender.ExtenderCarrierByteStats{
			extender.ExtenderCarrierTcp443: {
				Carrier:        extender.ExtenderCarrierTcp443,
				IngressBytes:   1000,
				EgressBytes:    2000,
				IngressBytesV4: 800,
				IngressBytesV6: 200,
				EgressBytesV4:  1600,
				EgressBytesV6:  400,
				LiveConns:      3,
				LiveConnsV4:    2,
				LiveConnsV6:    1,
			},
		},
	}
	text := renderExtenderRoleMetrics(snapshot)

	expectedMetrics := []string{
		`urnet_extender_carrier_bytes_total{carrier="tcp-443",direction="ingress",family="v4"} 800`,
		`urnet_extender_carrier_bytes_total{carrier="tcp-443",direction="ingress",family="v6"} 200`,
		`urnet_extender_carrier_bytes_total{carrier="tcp-443",direction="egress",family="v4"} 1600`,
		`urnet_extender_carrier_bytes_total{carrier="tcp-443",direction="egress",family="v6"} 400`,
		`urnet_extender_carrier_connections{carrier="tcp-443",family="v4"} 2`,
		`urnet_extender_carrier_connections{carrier="tcp-443",family="v6"} 1`,
	}
	for _, expected := range expectedMetrics {
		if !strings.Contains(text, expected) {
			t.Errorf("metrics output missing expected line %q:\n%s", expected, text)
		}
	}

	// Verify cardinality: exactly 4 carriers * 2 directions * 2 families = 16 bytes series
	lines := strings.Split(text, "\n")
	byteSeriesCount := 0
	connSeriesCount := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "urnet_extender_carrier_bytes_total{") {
			byteSeriesCount++
		}
		if strings.HasPrefix(line, "urnet_extender_carrier_connections{") {
			connSeriesCount++
		}
	}
	if byteSeriesCount != 16 {
		t.Errorf("urnet_extender_carrier_bytes_total series count = %d, want 16", byteSeriesCount)
	}
	if connSeriesCount != 8 {
		t.Errorf("urnet_extender_carrier_connections series count = %d, want 8", connSeriesCount)
	}
}

func TestMergeCarrierByteStatsFamilySplit(t *testing.T) {
	base := map[string]extender.ExtenderCarrierByteStats{
		extender.ExtenderCarrierTcp443: {
			Carrier:        extender.ExtenderCarrierTcp443,
			IngressBytes:   500,
			EgressBytes:    600,
			IngressBytesV4: 400,
			IngressBytesV6: 100,
			EgressBytesV4:  500,
			EgressBytesV6:  100,
			ConnsTotal:     5,
			ConnsTotalV4:   4,
			ConnsTotalV6:   1,
		},
	}
	merged := mergeCarrierByteStats(base, nil, time.Now())
	tcp := merged[extender.ExtenderCarrierTcp443]
	if tcp.IngressBytesV4 != 400 || tcp.IngressBytesV6 != 100 || tcp.EgressBytesV4 != 500 || tcp.EgressBytesV6 != 100 {
		t.Fatalf("unexpected merged family byte counts: %+v", tcp)
	}
	if tcp.ConnsTotalV4 != 4 || tcp.ConnsTotalV6 != 1 {
		t.Fatalf("unexpected merged family conn counts: %+v", tcp)
	}
}
