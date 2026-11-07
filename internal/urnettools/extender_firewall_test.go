package urnettools

import (
	"reflect"
	"testing"
)

func TestExtenderFirewallUFWPlan(t *testing.T) {
	status := `Status: active

To                         Action      From
--                         ------      ----
443/tcp                    ALLOW IN    Anywhere
443/tcp (v6)               ALLOW IN    Anywhere (v6)
`
	got, err := extenderUFWPlan(status)
	if err != nil {
		t.Fatal(err)
	}
	want := []setupCommand{{name: "ufw", args: []string{"allow", "443/udp"}}, {name: "ufw", args: []string{"allow", "4053/udp"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v; want %v", got, want)
	}
}

func TestExtenderFirewallUFWDoesNotTrustRestrictedRules(t *testing.T) {
	status := "Status: active\n443/tcp ALLOW IN restricted-source\n443/udp DENY IN Anywhere\n4053/udp ALLOW OUT Anywhere\n"
	got, err := extenderUFWPlan(status)
	if err != nil || len(got) != 3 {
		t.Fatalf("plan = %v, err = %v", got, err)
	}
	if _, err := extenderUFWPlan("Status: inactive\n"); err == nil {
		t.Fatal("inactive firewall was treated as active")
	}
}
