package urnettools

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func renderExtender(t *testing.T, r ExtenderReading) string {
	t.Helper()
	var b bytes.Buffer
	formatExtenderBlock(&b, r, time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC))
	return b.String()
}

func TestExtenderBlockServing(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	out := renderExtender(t, ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", SettingSource: "default",
		Running: true, RoleState: "serving", Activating: true,
		Ports: []ExtenderPort{{Name: "tcp 443", Up: true}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{
			{IpVersion: 4, State: "activated", Ip: "203.0.113.5", ExpireUnixMs: now.Add(20 * time.Hour).UnixMilli()},
			{IpVersion: 6, State: "failed", Detail: "no public IPv6 address", NextAttemptUnixMs: now.Add(23 * time.Minute).UnixMilli()},
		},
		LiveClients: 2,
		Served:      ExtenderServed{Last24h: 120, Last7d: 800, Lifetime: 5000},
	})
	for _, want := range []string{
		"Extender:", "serving",
		"tcp 443 up", "udp 443 up", "dns 4053 up",
		"IPv4", "ACTIVE", "203.0.113.5",
		"IPv6", "not active", "no public IPv6 address", "next try " + now.Add(23*time.Minute).Local().Format("15:04"),
		"Clients now", "2",
		"24h 120", "7d 800", "lifetime 5000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block is missing %q:\n%s", want, out)
		}
	}
}

func TestExtenderBlockOffSaysHowToTurnOn(t *testing.T) {
	out := renderExtender(t, ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "off",
		SettingSource: "control socket (urnet-tools extender off)", RoleState: "off",
		OffReason: "turned off by control socket (urnet-tools extender off). Turn it back on with: urnet-tools extender on",
		Served:    ExtenderServed{Lifetime: 9},
	})
	for _, want := range []string{"OFF", "urnet-tools extender on", "lifetime 9"} {
		if !strings.Contains(out, want) {
			t.Errorf("off block is missing %q:\n%s", want, out)
		}
	}
}

func TestExtenderBlockCallsOutBlockedTcp(t *testing.T) {
	out := renderExtender(t, ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", Running: true, RoleState: "tcp_unavailable",
		Ports:    []ExtenderPort{{Name: "tcp 443", Up: false, Error: "address already in use"}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}},
		Families: []ExtenderFamily{{IpVersion: 4, State: "held"}, {IpVersion: 6, State: "held"}},
	})
	for _, want := range []string{"tcp 443 DOWN", "address already in use", "NOT listed", "held"} {
		if !strings.Contains(out, want) {
			t.Errorf("blocked block is missing %q:\n%s", want, out)
		}
	}
}

// A provider that cannot answer is reported as such, never as "off".
func TestExtenderBlockUnavailable(t *testing.T) {
	out := renderExtender(t, ExtenderReading{Source: SourceUnknown})
	if !strings.Contains(out, "unavailable") || strings.Contains(out, "OFF") {
		t.Errorf("an unreachable provider must read unavailable, not off:\n%s", out)
	}
}

func TestExtenderSetKeyIsKnown(t *testing.T) {
	got, ok := canonicalControlKey("extender")
	if !ok || got != "extender" {
		t.Fatalf("canonicalControlKey(extender) = %q, %v", got, ok)
	}
	if err := validateControlValue("extender", "on"); err != nil {
		t.Errorf("on rejected: %v", err)
	}
	if err := validateControlValue("extender", "maybe"); err == nil {
		t.Error("maybe accepted")
	}
}

// `extender off` must persist a real off. Treated as a clear it would revert to
// the default, which is ON, and the extender would silently stay on.
func TestExtenderOffIsNotAClear(t *testing.T) {
	if treatsOffAsClear("extender") {
		t.Fatal("`extender off` would clear the setting and return to the default, which is on")
	}
}

// `enable` and `disable` are plain-language names for on and off.
func TestExtenderActionAliases(t *testing.T) {
	tests := map[string]string{
		"status": "status", "on": "on", "off": "off",
		"enable": "on", "disable": "off",
		"ENABLE": "on", " Disable ": "off",
	}
	for in, want := range tests {
		got, ok := normalizeExtenderAction(in)
		if !ok || got != want {
			t.Errorf("normalizeExtenderAction(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "maybe", "start", "enabled"} {
		if _, ok := normalizeExtenderAction(bad); ok {
			t.Errorf("normalizeExtenderAction(%q) was accepted", bad)
		}
	}
}
