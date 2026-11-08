package urnettools

import (
	"errors"
	"reflect"
	"testing"
)

func TestFreeBSDPortInspectionUsesSockstat(t *testing.T) {
	var calls [][]string
	checker := &PortChecker{Platform: "freebsd", ExecCmd: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if name != "sockstat" {
			return nil, errors.New("not installed")
		}
		if len(args) > 2 && args[2] == "tcp" {
			return []byte("USER COMMAND PID FD PROTO LOCAL ADDRESS FOREIGN ADDRESS\nroot unrelated 9 3 tcp4 10.0.0.1:4444 10.0.0.2:443\nroot web 123 4 tcp6 *:443 *:*\n"), nil
		}
		return []byte("USER COMMAND PID FD PROTO LOCAL ADDRESS FOREIGN ADDRESS\nnobody dns 321 7 udp4 *:4053 *:*\nuser other 88 6 udp4 1.2.3.4:4430 8.8.8.8:443\nuser quic 55 8 udp6 :::443 *:*\n"), nil
	}}
	got, err := checker.CheckPorts()
	if err != nil {
		t.Fatal(err)
	}
	if !got.TCP443.Held || got.TCP443.PID != 123 || got.TCP443.Process != "web" || !got.UDP443.Held || got.UDP443.PID != 55 || !got.UDP4053.Held || got.UDP4053.PID != 321 {
		t.Fatalf("ports=%+v", got)
	}
	want := [][]string{{"sockstat", "-46l", "-P", "tcp", "-p", "443"}, {"sockstat", "-46", "-P", "udp", "-p", "443,4053"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v", calls)
	}
}
func TestFreeBSDPortInspectionErrors(t *testing.T) {
	failure := errors.New("unavailable")
	c := &PortChecker{Platform: "freebsd", ExecCmd: func(string, ...string) ([]byte, error) { return nil, failure }}
	_, err := c.CheckPorts()
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
}
