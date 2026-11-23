package urnettools

import (
	"strings"
	"testing"
)

func TestLingerLifecycleWarning(t *testing.T) {
	old := statusLingerCheckFn
	t.Cleanup(func() { statusLingerCheckFn = old })
	calls := 0
	statusLingerCheckFn = func(user string) (bool, error) {
		calls++
		if user != "alice" {
			t.Errorf("user = %s", user)
		}
		return false, nil
	}
	p := Provider{User: "alice", Unit: "urnet-linger-test.service"}
	if got := lifecycleLingerWarning(p); !strings.Contains(got, "sudo loginctl enable-linger alice") {
		t.Fatalf("warning = %q", got)
	}
	for _, p := range []Provider{{User: "alice"}, {User: "alice", Unit: "urnet-linger-test.service", Supervisor: "openrc"}} {
		if got := lifecycleLingerWarning(p); got != "" {
			t.Fatalf("unexpected warning %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("checks = %d", calls)
	}
}
