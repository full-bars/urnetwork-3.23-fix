//go:build linux

package urnettools

import (
	"encoding/json"
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

// The default status view is the one most people run. It prints the update
// line and the health line, and a user service with lingering off needs its
// warning there too, not only in the one-screen summary.
func TestStatusVerdictPrintsLingerWarning(t *testing.T) {
	oldLinger := statusLingerCheckFn
	oldSnap := fetchSnapshotFn
	oldUpdate := checkStatusUpdateFn
	t.Cleanup(func() {
		statusLingerCheckFn = oldLinger
		fetchSnapshotFn = oldSnap
		checkStatusUpdateFn = oldUpdate
	})
	fetchSnapshotFn = func(Provider) (*NodeSnapshot, json.RawMessage, error) { return nil, nil, errors.New("no snapshot") }
	checkStatusUpdateFn = func(string) statusUpdateState { return statusUpdateState{} }
	p := Provider{Unit: "test-linger-unknown.service", User: "alice"}

	for _, tc := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{"off", false, true},
		{"on", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusLingerCheckFn = func(string) (bool, error) { return tc.enabled, nil }
			out := captureStdout(t, func() { printStatusVerdict(p, true) })
			has := strings.Contains(out, "sudo loginctl enable-linger alice")
			if has != tc.want {
				t.Fatalf("warning present = %v, want %v in:\n%s", has, tc.want, out)
			}
		})
	}
}
