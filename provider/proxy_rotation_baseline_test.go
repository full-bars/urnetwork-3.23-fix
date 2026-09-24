package main

import (
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestRunningAuthBaselineSurvivesLaunchMutation is the regression test for the
// perpetual credential-rotation loop: the rotation baseline (runningAuth) must
// be a deep copy of the configured settings, never the pointer handed to the
// launched goroutine. The proxy runtime mutates the launched settings during
// auth, and a shared pointer made the next reload see a credential change that
// never happened — rotating every credentialed proxy on every reload.
func TestRunningAuthBaselineSurvivesLaunchMutation(t *testing.T) {
	r := &ProxyReloader{cancelMapMu: &sync.Mutex{}}

	config := &connect.ProxySettings{
		Network: "tcp",
		Address: "dc.decodo.com:10001",
		Auth:    &proxy.Auth{User: "user1", Password: "pass1"},
	}
	r.seedRunningAuth([]*connect.ProxySettings{config})

	// Emulate the runtime's auth write-back on the pointer it was launched with.
	config.Auth.Password = "MUTATED-BY-RUNTIME"

	// Next reload parses the config fresh; the two must still match.
	fresh := &connect.ProxySettings{
		Network: "tcp",
		Address: "dc.decodo.com:10001",
		Auth:    &proxy.Auth{User: "user1", Password: "pass1"},
	}
	rec, ok := r.runningAuthFor(config.Key())
	if !ok {
		t.Fatalf("expected recorded baseline for identity key %q", config.Key())
	}
	if rec.Auth == config.Auth {
		t.Fatal("baseline shares the Auth pointer with the launched settings")
	}
	if !sameAuth(rec, fresh) {
		t.Fatalf("baseline poisoned by launch mutation: recorded=%+v fresh=%+v", rec, fresh)
	}
}

// TestCloneProxySettingsDeepCopiesAuth guards the copy helper itself.
func TestCloneProxySettingsDeepCopiesAuth(t *testing.T) {
	s := &connect.ProxySettings{
		Network: "tcp",
		Address: "dc.decodo.com:10001",
		Auth:    &proxy.Auth{User: "u", Password: "p"},
	}
	c := cloneProxySettings(s)
	if c == s {
		t.Fatal("clone returned the same pointer")
	}
	if c.Auth == s.Auth {
		t.Fatal("clone shares the Auth pointer")
	}
	c.Auth.Password = "changed"
	if s.Auth.Password != "p" {
		t.Fatal("mutating the clone changed the original")
	}
	if cloneProxySettings(nil) != nil {
		t.Fatal("nil clone should be nil")
	}
}
