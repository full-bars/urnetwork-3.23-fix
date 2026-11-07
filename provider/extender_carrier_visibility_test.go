package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestExtenderHealthCarrierVisibility(t *testing.T) {
	ports := []extenderPortReading{{Name: "tcp 443", Up: true}, {Name: "udp 443", Up: true}, {Name: "dns 4053", Up: true}}
	family := &connect.ExtenderFamilyActivationStatus{
		Activated:       true,
		Carriers:        []string{connect.ExtenderCarrierTcp},
		RefusedCarriers: []connect.ExtenderCarrierPort{{Carrier: connect.ExtenderCarrierQuic, Port: 443}, {Carrier: connect.ExtenderCarrierDns, Port: 4053}},
	}
	reading := extenderControlReading{Known: true, Running: true, RoleState: extenderStateServing,
		Ports: ports, Families: []extenderFamilyReading{{IpVersion: 4, State: extenderFamilyActivated, Ports: extenderFamilyPorts(ports, family, true)}},
	}
	line := formatExtenderHealthLine(reading, time.Time{})
	for _, want := range []string{"tcp 443 bound, accepted by the platform", "udp 443 bound, left out by the platform", "dns 4053 bound, left out by the platform"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
	family.Carriers = []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns}
	family.DnsPorts = []int{4053}
	family.RefusedCarriers = nil
	reading.Families[0].Ports = extenderFamilyPorts(ports, family, true)
	line = formatExtenderHealthLine(reading, time.Time{})
	if strings.Contains(line, "left out") || !strings.Contains(line, "udp 443 bound, accepted by the platform") {
		t.Fatalf("recovery: %s", line)
	}
}
