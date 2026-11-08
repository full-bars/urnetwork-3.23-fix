package urnettools

import (
	"errors"
	"reflect"
	"testing"
)

func TestLinuxCapabilityGrantPreservesExisting(t *testing.T) {
	binary := "/some path/provider"
	var calls [][]string
	sp := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "getcap" {
			if len(calls) > 2 {
				return []byte(binary + " cap_net_bind_service,cap_net_raw=ep\n"), nil
			}
			return []byte(binary + " cap_net_raw=ep\n"), nil
		}
		return nil, nil
	}}
	plan, err := sp.PlanRequirement(binary, "provider")
	if err != nil {
		t.Fatal(err)
	}
	want := []setupCommand{{name: "setcap", args: []string{"cap_net_raw=ep cap_net_bind_service+ep", binary}}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan=%v want=%v", plan, want)
	}
	calls = nil
	ok, _, err := sp.applyLinux(binary)
	if !ok || err != nil {
		t.Fatalf("apply=%v err=%v calls=%v", ok, err, calls)
	}
	if len(calls) != 3 || !reflect.DeepEqual(calls[1], []string{"setcap", want[0].args[0], binary}) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestLinuxCapabilityGrantNamespaceAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   []string
		bad    bool
	}{
		{"/provider cap_net_raw=ep [rootid=1234]", []string{"-n", "1234", "cap_net_raw=ep cap_net_bind_service+ep", "/provider"}, false},
		{"/provider =", []string{"= cap_net_bind_service+ep", "/provider"}, false},
		{"/provider cap_net_raw=ep [rootid=oops]", nil, true},
		{"/provider cap_net_raw=ep unexpected", nil, true},
	} {
		sp := &SystemPlatformConfigurer{ExecCmd: func(string, ...string) ([]byte, error) { return []byte(tc.output), nil }}
		got, err := sp.linuxCapabilityArgs("/provider")
		if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("output=%q args=%v err=%v", tc.output, got, err)
		}
	}
}

func TestLinuxCapabilityGrantFailsClosedOnUnreadable(t *testing.T) {
	count := 0
	sp := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
		if name == "setcap" {
			count++
		}
		return nil, errors.New("getcap failed")
	}}
	if _, err := sp.PlanRequirement("/provider", "user"); err == nil {
		t.Fatal("planned without existing capabilities")
	}
	if ok, _, err := sp.applyLinux("/provider"); ok || err == nil || count != 0 {
		t.Fatalf("ok=%v err=%v setcap=%d", ok, err, count)
	}
}
