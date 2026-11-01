package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The relay-dial error used to be wrapped with %w. dialSocks5UDP hands the dial
// error straight back, and a net error echoes the address it was given, so a
// malformed proxy address (userinfo or a token path) came through the redaction
// and into the operator log. These tests pin the message, not the wording.
const (
	leakUser   = "alice"
	leakSecret = "sekretpw"
	leakToken  = "tok3npath"
)

func credentialsLeakingAddress() string {
	return "socks5://" + leakUser + ":" + leakSecret + "@10.1.2.3:1080/" + leakToken
}

// TestDialFailureClassCarriesNoAddress pins the helper that replaced %w: a
// net.OpError keeps the dialed address verbatim, and the class must not.
func TestDialFailureClassCarriesNoAddress(t *testing.T) {
	addr := credentialsLeakingAddress()
	err := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("dial %s: connect: connection refused", addr)}

	got := dialFailureClass(err)
	if got == "" {
		t.Fatal("dialFailureClass returned an empty class")
	}
	for _, leak := range []string{leakUser, leakSecret, leakToken, "10.1.2.3"} {
		if strings.Contains(got, leak) {
			t.Fatalf("dial failure class leaks %q: %q", leak, got)
		}
	}
}

// TestH3PacketConnFactoryErrorCarriesNoCredentials is the end-to-end check
// CodeRabbit asked for: a malformed proxy address must not surface its
// credentials in the error the factory returns.
func TestH3PacketConnFactoryErrorCarriesNoCredentials(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: credentialsLeakingAddress()}
	factory := newH3PacketConnFactory(settings)
	if factory == nil {
		t.Fatal("a proxied identity must get a factory")
	}

	conn, err := factory(context.Background())
	if conn != nil {
		defer conn.Close()
	}
	if err == nil {
		t.Skip("the dial unexpectedly succeeded; nothing to assert about the error")
	}
	msg := err.Error()
	for _, leak := range []string{leakUser, leakSecret, leakToken} {
		if strings.Contains(msg, leak) {
			t.Fatalf("factory error leaks %q: %s", leak, msg)
		}
	}
}
