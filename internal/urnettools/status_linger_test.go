//go:build linux

package urnettools

import (
	"errors"
	"strings"
	"testing"
)

func TestStatusLingerWarning(t *testing.T) {
	old := statusLingerCheckFn
	t.Cleanup(func() { statusLingerCheckFn = old })
	for _, tc := range []struct {
		name    string
		p       Provider
		enabled bool
		err     error
		warn    bool
	}{
		{"off", Provider{Unit: "test-linger-unknown.service", User: "alice"}, false, nil, true},
		{"on", Provider{Unit: "test-linger-unknown.service", User: "alice"}, true, nil, false},
		{"unavailable", Provider{Unit: "test-linger-unknown.service", User: "alice"}, false, errors.New("unavailable"), false},
		{"openrc", Provider{Unit: "test-linger-unknown.service", User: "alice", Supervisor: "openrc"}, false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusLingerCheckFn = func(user string) (bool, error) {
				if user != "alice" {
					t.Errorf("user = %q", user)
				}
				return tc.enabled, tc.err
			}
			found := false
			for _, row := range collectStatusData(tc.p).Rows {
				if row.Name == "Linger" {
					found = true
					if row.Level != statusWarn || !strings.Contains(row.Text, "sudo loginctl enable-linger alice") {
						t.Errorf("row = %#v", row)
					}
				}
			}
			if found != tc.warn {
				t.Errorf("warning = %v, want %v", found, tc.warn)
			}
		})
	}
}
