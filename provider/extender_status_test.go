package main

import (
	"strings"
	"testing"
	"time"
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
		"tcp 443 up", "udp 443 up", "dns 4053 up",
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

// A blocked tcp 443 is the failure that kept a production host off the platform. The line
// must say so in words, not leave the operator to infer it from a missing "up".
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
