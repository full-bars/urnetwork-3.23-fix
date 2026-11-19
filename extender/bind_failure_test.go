package extender

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestClassifyBindError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil":                {nil, ""},
		"address in use":     {&net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}, BindFailureAddressInUse},
		"wrapped in use":     {fmt.Errorf("udp 53: %w", syscall.EADDRINUSE), BindFailureAddressInUse},
		"permission":         {&net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EACCES)}, BindFailurePermissionDenied},
		"operation not perm": {fmt.Errorf("udp 53: %w", syscall.EPERM), BindFailurePermissionDenied},
		"os permission":      {fmt.Errorf("udp 53: %w", os.ErrPermission), BindFailurePermissionDenied},
		"other errno":        {syscall.ENETDOWN, BindFailureUnknown},
		"text only":          {errors.New("address already in use"), BindFailureUnknown},
	} {
		if got := ClassifyBindError(tc.err); got != tc.want {
			t.Errorf("%s: ClassifyBindError = %q, want %q", name, got, tc.want)
		}
	}
}

// A real second bind of one udp port is classified as an address in use.
func TestClassifyBindErrorRealCollision(t *testing.T) {
	first, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback udp: %v", err)
	}
	defer first.Close()
	_, err = net.ListenPacket("udp", first.LocalAddr().String())
	if err == nil {
		t.Skip("platform allows a second bind of the same udp port")
	}
	if got := ClassifyBindError(err); got != BindFailureAddressInUse {
		t.Fatalf("ClassifyBindError(%v) = %q, want %q", err, got, BindFailureAddressInUse)
	}
}

func TestUdpPortListenErrorsFollowTheLastBind(t *testing.T) {
	server := &ExtenderServer{udpPortListenErrs: map[int]error{}}
	server.setUdpPortListenError(53, syscall.EADDRINUSE)
	server.setUdpPortListenError(4053, nil)
	got := server.UdpPortListenErrors()
	if len(got) != 1 || ClassifyBindError(got[53]) != BindFailureAddressInUse {
		t.Fatalf("errors = %v, want only port 53 in use", got)
	}
	got[53] = nil // callers get a copy
	if server.UdpPortListenErrors()[53] == nil {
		t.Fatal("UdpPortListenErrors returned the live map")
	}
	server.setUdpPortListenError(53, nil)
	if len(server.UdpPortListenErrors()) != 0 {
		t.Fatalf("a port that bound later kept its failure: %v", server.UdpPortListenErrors())
	}
}
