package urnettools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFirewalldDetectionPlansRuntimeAndPermanentWithoutReload(t *testing.T) {
	var writes []setupCommand
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(name string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--query-port=") {
				if arg == "--query-port=443/tcp" && args[0] != "--permanent" {
					return []byte("yes"), nil
				}
				return []byte("no"), errors.New("not enabled")
			}
		}
		writes = append(writes, setupCommand{name: name, args: append([]string(nil), args...)})
		return nil, nil
	}}
	status := fm.Detect()
	want := []setupCommand{
		{name: "firewall-cmd", args: []string{"--permanent", "--zone=public", "--add-port=443/tcp"}},
		{name: "firewall-cmd", args: []string{"--zone=public", "--add-port=443/udp"}},
		{name: "firewall-cmd", args: []string{"--permanent", "--zone=public", "--add-port=443/udp"}},
		{name: "firewall-cmd", args: []string{"--zone=public", "--add-port=4053/udp"}},
		{name: "firewall-cmd", args: []string{"--permanent", "--zone=public", "--add-port=4053/udp"}},
	}
	if status.PortsOpen || !reflect.DeepEqual(fm.Plan(status), want) {
		t.Fatalf("status=%+v plan=%v", status, fm.Plan(status))
	}
	ok, err := fm.Apply(status)
	if !ok || err != nil || !reflect.DeepEqual(writes, want) {
		t.Fatalf("applied=%v err=%v writes=%v", ok, err, writes)
	}
}

func TestFirewalldUnknownStateFailsClosed(t *testing.T) {
	var writes int
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return nil, errors.New("denied")
		}
		writes++
		return nil, nil
	}}
	status := fm.Detect()
	if status.Kind != FirewallFirewalld || status.PortsOpen || len(fm.Plan(status)) != 0 {
		t.Fatalf("unsafe plan for unknown state: %+v %v", status, fm.Plan(status))
	}
	ok, err := fm.Apply(status)
	if ok || err == nil || writes != 0 {
		t.Fatalf("applied=%v err=%v writes=%d", ok, err, writes)
	}
	ok, err = fm.Apply(FirewallStatus{Kind: FirewallFirewalld})
	if ok || err == nil || writes != 0 {
		t.Fatalf("empty plan applied=%v err=%v writes=%d", ok, err, writes)
	}
}
