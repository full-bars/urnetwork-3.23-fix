package main

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
