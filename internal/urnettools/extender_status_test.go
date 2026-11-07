package urnettools

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
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
		"tcp 443 bound", "udp 443 bound", "dns 4053 bound",
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
	for _, want := range []string{"tcp 443 not bound", "address already in use", "NOT listed", "held"} {
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

// With the provider down, `extender off` queues the change; that is a success, so
// the command must not then fail because it cannot read a live status.
func TestExtenderChangeWithProviderDownSucceeds(t *testing.T) {
	prev := socketExtenderFn
	t.Cleanup(func() { socketExtenderFn = prev })
	socketExtenderFn = func(Provider) (*ExtenderReading, error) {
		return nil, extenderSocketError("/x/provider.sock", syscall.ECONNREFUSED)
	}
	var out bytes.Buffer
	if err := finishExtenderChange(&out, Provider{StateDir: "/x"}, "off"); err != nil {
		t.Fatalf("a queued change must succeed while the provider is down, got %v", err)
	}
	if !strings.Contains(out.String(), "applies when the provider starts") {
		t.Fatalf("it must say the change applies at the next start, got %q", out.String())
	}
}

// A provider that is up but cannot answer is a real problem and still fails.
func TestExtenderChangeWithAnUnansweringProviderStillFails(t *testing.T) {
	prev := socketExtenderFn
	t.Cleanup(func() { socketExtenderFn = prev })
	socketExtenderFn = func(Provider) (*ExtenderReading, error) {
		return nil, errors.New("control socket unavailable: timeout")
	}
	var out bytes.Buffer
	if err := finishExtenderChange(&out, Provider{StateDir: "/x"}, "on"); err == nil {
		t.Fatal("an unanswering provider must still return an error")
	}
}

func TestCmdExtenderCrossUserElevation(t *testing.T) {
	tempDir := t.TempDir()
	origProc, origStop := discoverProcessesFn, discoverStoppedFn
	origPriv := isPrivileged
	origE := elevateSelfFunc
	t.Cleanup(func() {
		discoverProcessesFn, discoverStoppedFn = origProc, origStop
		isPrivileged = origPriv
		elevateSelfFunc = origE
	})
	discoverProcessesFn = func() []Provider {
		return []Provider{{Unit: "other.service", User: "other-user", StateDir: tempDir}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	isPrivileged = func() bool { return false }

	var elevatedArgv []string
	elevateSelfFunc = func(args []string) error {
		elevatedArgv = append([]string{}, args...)
		return nil
	}

	// Normal run targeting another user must re-execute under sudo.
	if err := cmdExtender([]string{"off"}, false); err != nil {
		t.Fatalf("cmdExtender failed: %v", err)
	}
	if len(elevatedArgv) == 0 || elevatedArgv[0] != "extender" {
		t.Fatalf("expected elevation with subcommand 'extender', got %v", elevatedArgv)
	}

	// Dry run must NOT elevate.
	elevatedArgv = nil
	if err := cmdExtender([]string{"off"}, true); err != nil {
		t.Fatalf("cmdExtender dry-run failed: %v", err)
	}
	if len(elevatedArgv) != 0 {
		t.Fatalf("dry run must not elevate, got %v", elevatedArgv)
	}
}

func TestExtenderAboutPrintsWithoutAProvider(t *testing.T) {
	for _, word := range []string{"about", "help", "explain"} {
		out := captureStdout(t, func() {
			if err := cmdExtender([]string{word}, false); err != nil {
				t.Fatalf("%s: %v", word, err)
			}
		})
		for _, want := range []string{"tcp 443", "udp 4053", "udp 53", "extender off"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output is missing %q", word, want)
			}
		}
	}
}
