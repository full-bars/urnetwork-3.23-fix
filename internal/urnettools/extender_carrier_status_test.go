package urnettools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtenderCarrierPortStatus(t *testing.T) {
	for _, tc := range []struct{ name, portJSON, want string }{
		{"bound", `{"name":"udp 443","up":true}`, "udp 443 bound"},
		{"failed", `{"name":"udp 443","error":"address in use"}`, "udp 443 not bound (address in use)"},
		{"not reported", `{"name":"udp 443"}`, "udp 443 not bound"},
		{"disabled", `{"name":"udp 443","disabled":true}`, "udp 443 disabled by setting"},
		{"dns 53 in use", `{"name":"dns 53","reason":"address_in_use"}`, "dns 53 not bound (address already in use)"},
		{"dns 53 permission", `{"name":"dns 53","reason":"permission_denied"}`, "dns 53 not bound (permission denied)"},
		{"dns 53 not requested", `{"name":"dns 53","reason":"not_requested"}`, "dns 53 not requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var port ExtenderPort
			if err := json.Unmarshal([]byte(tc.portJSON), &port); err != nil {
				t.Fatal(err)
			}
			out := renderExtender(t, ExtenderReading{Source: SourceControlSocket, Running: true, Ports: []ExtenderPort{port}, Families: []ExtenderFamily{{IpVersion: 4, Ports: []ExtenderFamilyPort{{Name: "udp 443", State: tc.want[len("udp 443 "):]}}}}})
			if !strings.Contains(out, "Ports:             "+tc.want) {
				t.Fatalf("unexpected summary: %s", out)
			}
			if tc.name == "disabled" && !strings.Contains(out, "udp 443: disabled by setting") {
				t.Fatalf("unexpected family state: %s", out)
			}
		})
	}
}
