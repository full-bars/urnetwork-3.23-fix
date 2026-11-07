package urnettools

import "testing"

func TestParseLsofLocalEndpointOnly(t *testing.T) {
	cases := []struct {
		name string
		line string
		held bool
	}{
		{"remote port", "browser 12 user 4u IPv4 1 0t0 UDP 192.0.2.1:55000->192.0.2.2:443", false},
		{"port prefix", "other 13 user 4u IPv4 1 0t0 UDP *:4430", false},
		{"local port", "provider 14 user 4u IPv6 1 0t0 UDP [::1]:443->192.0.2.2:55000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var holder PortHolder
			parseLsofOutput(tc.line, &holder, 443)
			if holder.Held != tc.held {
				t.Fatalf("held = %v, want %v", holder.Held, tc.held)
			}
		})
	}
}
