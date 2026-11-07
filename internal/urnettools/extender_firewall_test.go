package urnettools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderFirewallFirewalldPlan(t *testing.T) {
	var calls [][]string
	plan, err := extenderFirewalldPlan(func(name string, args ...string) ([]byte, error) {
		if name != "firewall-cmd" {
			t.Fatalf("unexpected command %q", name)
		}
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"--get-default-zone"}) {
			return []byte("public\n"), nil
		}
		for _, arg := range args {
			if arg == "--query-port=443/tcp" {
				return []byte("yes\n"), nil
			}
		}
		return []byte("no\n"), errors.New("not enabled")
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []setupCommand{
		{name: "firewall-cmd", args: []string{"--zone=public", "--add-port=443/udp"}},
		{name: "firewall-cmd", args: []string{"--permanent", "--zone=public", "--add-port=443/udp"}},
		{name: "firewall-cmd", args: []string{"--zone=public", "--add-port=4053/udp"}},
		{name: "firewall-cmd", args: []string{"--permanent", "--zone=public", "--add-port=4053/udp"}},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan=%v want=%v", plan, want)
	}
	if len(calls) != 7 {
		t.Fatalf("read calls=%v", calls)
	}
}

func TestExtenderFirewallFirewalldRejectsUnknown(t *testing.T) {
	_, err := extenderFirewalldPlan(func(name string, args ...string) ([]byte, error) {
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		return []byte("permission denied"), errors.New("failed")
	})
	if err == nil {
		t.Fatal("unknown rule state accepted")
	}
}

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

func TestExtenderFirewallUFWDetectMissingUDPOnly(t *testing.T) {
	status := `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW       Anywhere
22/tcp (v6)                ALLOW       Anywhere (v6)
443/tcp (v6)               ALLOW       Anywhere (v6)
`
	var executed []setupCommand
	fm := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) {
			if file == "ufw" {
				return "/usr/sbin/ufw", nil
			}
			return "", errors.New("not found")
		},
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ufw" && len(args) == 1 && args[0] == "status" {
				return []byte(status), nil
			}
			executed = append(executed, setupCommand{name: name, args: append([]string(nil), args...)})
			return nil, nil
		},
	}
	st := fm.Detect()
	if st.Kind != FirewallUFW || !st.Active || st.PortsOpen {
		t.Fatalf("unexpected status: %+v", st)
	}
	wantPlan := []setupCommand{
		{name: "ufw", args: []string{"allow", "443/udp"}},
		{name: "ufw", args: []string{"allow", "4053/udp"}},
	}
	if !reflect.DeepEqual(fm.Plan(st), wantPlan) {
		t.Fatalf("Plan() = %v, want %v", fm.Plan(st), wantPlan)
	}
	if !reflect.DeepEqual(st.SuggestedRules, []string{"ufw allow 443/udp", "ufw allow 4053/udp"}) {
		t.Fatalf("SuggestedRules = %v", st.SuggestedRules)
	}
	applied, err := fm.Apply(st)
	if err != nil || !applied {
		t.Fatalf("Apply() = %v, %v", applied, err)
	}
	if !reflect.DeepEqual(executed, wantPlan) {
		t.Fatalf("executed = %v, want %v", executed, wantPlan)
	}
}

func TestExtenderFirewallIptablesAndNftablesSuggestedRules(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		kind   FirewallKind
		expect string
	}{
		{tool: "nft", kind: FirewallNftables, expect: "nft add rule inet filter input udp dport { 443, 4053 } accept"},
		{tool: "iptables", kind: FirewallIptables, expect: "iptables -I INPUT -p udp --dport 4053 -j ACCEPT"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			fm := &FirewallManager{
				Platform: "linux",
				LookPath: func(file string) (string, error) {
					if file == tc.tool || file == "systemctl" {
						return file, nil
					}
					return "", errors.New("not found")
				},
				ExecCmd: func(name string, args ...string) ([]byte, error) {
					return []byte("active\n"), nil
				},
			}
			st := fm.Detect()
			if st.Kind != tc.kind || !st.Active || st.PortsOpen {
				t.Fatalf("unexpected status: %+v", st)
			}
			if len(fm.Plan(st)) != 0 {
				t.Fatalf("expected no auto-execution plan for %s, got %v", tc.tool, fm.Plan(st))
			}
			joined := strings.Join(st.SuggestedRules, "\n")
			if !strings.Contains(joined, tc.expect) {
				t.Fatalf("missing suggested rule %q in %q", tc.expect, joined)
			}
		})
	}
}
