package main

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

type probeListener struct {
	closed   bool
	closeErr error
}

func (l *probeListener) Accept() (net.Conn, error) { return nil, errors.New("unexpected accept") }
func (l *probeListener) Close() error              { l.closed = true; return l.closeErr }
func (l *probeListener) Addr() net.Addr            { return nil }

func TestExtenderBindProbe(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		args                             []string
		listenErr                        error
		closeErr                         error
		handled, success, called, closed bool
		wantOut, wantErr                 string
	}{
		{name: "other command", args: []string{"provide"}},
		{name: "extra argument", args: []string{"--extender-bind-probe", "extra"}},
		{name: "success", args: []string{"--extender-bind-probe"}, handled: true, success: true, called: true, closed: true, wantOut: "extender-bind-probe: proven\n"},
		{name: "listen failure", args: []string{"--extender-bind-probe"}, listenErr: errors.New("denied"), handled: true, called: true, wantErr: "extender-bind-probe: failed: denied\n"},
		{name: "close failure", args: []string{"--extender-bind-probe"}, closeErr: errors.New("close failed"), handled: true, called: true, closed: true, wantErr: "extender-bind-probe: failed: close failed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			l := &probeListener{closeErr: tc.closeErr}
			called := false
			listen := func(network, address string) (net.Listener, error) {
				called = true
				if network != "tcp" || address != ":443" {
					t.Fatalf("unexpected bind %s %s", network, address)
				}
				if tc.listenErr != nil {
					return nil, tc.listenErr
				}
				return l, nil
			}
			handled, success := handleExtenderBindProbe(tc.args, listen, &out, &errOut)
			if handled != tc.handled || success != tc.success || called != tc.called || l.closed != tc.closed {
				t.Fatalf("handled=%t success=%t called=%t closed=%t", handled, success, called, l.closed)
			}
			if out.String() != tc.wantOut || errOut.String() != tc.wantErr {
				t.Fatalf("out=%q err=%q", strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()))
			}
		})
	}
}
