//go:build openbsd

package urnettools

import "testing"

const testOpenBSDBinary = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"

func TestOpenBSDServiceControlRejectsWrongProcess(t *testing.T) {
	oldPID, oldRun := openbsdServicePID, bsdServiceRunFn
	t.Cleanup(func() { openbsdServicePID, bsdServiceRunFn = oldPID, oldRun })
	openbsdServicePID = func() int { return 123 }
	calls := 0
	bsdServiceRunFn = func(string, ...string) ([]byte, error) { calls++; return nil, nil }
	for _, action := range []string{"stop", "restart", "start"} {
		for _, p := range []Provider{
			{PID: 456, Supervisor: "openbsd-rcd", Binary: testOpenBSDBinary},
			{PID: 123, Binary: testOpenBSDBinary},
			{PID: 123, Supervisor: "openbsd-rcd"},
		} {
			if err := bsdServiceControl(p, action); err == nil {
				t.Fatalf("%s accepted %+v", action, p)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("invoked service command %d times", calls)
	}
	if err := openbsdRequireManaged(Provider{PID: 123, Supervisor: "openbsd-rcd"}); err != nil {
		t.Fatalf("matching service rejected: %v", err)
	}
	openbsdServicePID = func() int { return 0 }
	if err := openbsdRequireManaged(Provider{PID: 123, Supervisor: "openbsd-rcd"}); err == nil {
		t.Fatal("stale identity accepted")
	}
}

// After uninstall stops the provider its PID is gone, so cleanup must rely on
// the identity recorded at discovery and never touch an unmanaged provider.
func TestOpenBSDCleanupServiceSkipsUnmanaged(t *testing.T) {
	oldRun := bsdServiceRunFn
	t.Cleanup(func() { bsdServiceRunFn = oldRun })
	calls := 0
	bsdServiceRunFn = func(string, ...string) ([]byte, error) { calls++; return nil, nil }
	for _, p := range []Provider{
		{PID: 123, Binary: testOpenBSDBinary},
		{PID: 123, Supervisor: "openbsd-rcd"},
		{},
	} {
		if err := openbsdCleanupService(p); err == nil {
			t.Fatalf("cleanup accepted %+v", p)
		}
	}
	if calls != 0 {
		t.Fatalf("invoked service command %d times", calls)
	}
}
