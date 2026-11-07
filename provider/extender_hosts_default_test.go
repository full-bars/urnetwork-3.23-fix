package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The platform proves an extender by forwarding a real request to its own api
// host through it, and the extender refuses any host not on its allow-list. With
// an empty list the required test can never pass, so the platform's own space is
// allowed by default (as the sdk does).
func TestExtenderPlatformAllowedHosts(t *testing.T) {
	tests := []struct {
		apiUrl string
		want   []string
	}{
		{"https://api.bringyour.com", []string{"bringyour.com", "*.bringyour.com"}},
		{"https://api.bringyour.com/", []string{"bringyour.com", "*.bringyour.com"}},
		{"https://g2-api.bringyour.com", []string{"bringyour.com", "*.bringyour.com"}},
		{"https://api-v4.bringyour.com:443", []string{"bringyour.com", "*.bringyour.com"}},
		{"HTTPS://API.BringYour.COM", []string{"bringyour.com", "*.bringyour.com"}},
		{"https://example.co.uk", []string{"example.co.uk"}},
		{"https://example.github.io", []string{"example.github.io"}},
		{"https://api.example.co.uk", []string{"example.co.uk", "*.example.co.uk"}},
		// nothing to derive a space from: allow exactly that host, no wildcard
		{"http://127.0.0.1:8080", []string{"127.0.0.1"}},
		{"http://localhost:8080", []string{"localhost"}},
		{"", nil},
		{"::not a url::", nil},
	}
	for _, tc := range tests {
		got := extenderPlatformAllowedHosts(tc.apiUrl)
		if !slices.Equal(got, tc.want) {
			t.Errorf("extenderPlatformAllowedHosts(%q) = %v, want %v", tc.apiUrl, got, tc.want)
		}
	}
}

// An operator's own hosts are added to the platform's, never in place of them:
// replacing them would silently stop the extender from being activated.
func TestMergeExtenderHostsKeepsThePlatformAndDedupes(t *testing.T) {
	got := mergeExtenderHosts(
		[]string{"bringyour.com", "*.bringyour.com"},
		[]string{"example.org", "bringyour.com"},
	)
	want := []string{"bringyour.com", "*.bringyour.com", "example.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

// The role the manager starts allows the platform's hosts without any
// environment variable.
func TestExtenderManagerStartsTheRoleWithThePlatformHostsAllowed(t *testing.T) {
	withTempHome(t)
	m := newExtenderManager()
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.newConfig = func() extenderRoleConfig { return testExtenderRoleConfig(t) }
	m.SetApiUrl("https://api.bringyour.com")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx

	m.Reconcile("test")
	role := m.Role()
	if role == nil {
		t.Fatal("role did not start")
	}
	defer m.stop("test over")
	waitExtenderRoleReady(t, role)

	if !slices.Contains(role.config.allowedHosts, "*.bringyour.com") || !slices.Contains(role.config.allowedHosts, "bringyour.com") {
		t.Fatalf("allowed hosts = %v, want the platform's space and its wildcard", role.config.allowedHosts)
	}
	// the running server accepts the api host the platform's probe forwards to,
	// and still refuses a host that is not the platform's
	role.mu.Lock()
	servers := slices.Clone(role.servers)
	role.mu.Unlock()
	if len(servers) == 0 {
		t.Fatal("role has no running server")
	}
	if !servers[0].IsAllowedHost("api.bringyour.com") {
		t.Fatal("the running extender refuses the platform's own api host, so the platform's probe would fail")
	}
	if servers[0].IsAllowedHost("evil.example") {
		t.Fatal("the running extender forwards to a host that is not the platform's")
	}
	if got := role.forwardPolicySummary(); strings.Contains(got, "disabled") {
		t.Fatalf("forward policy reads %q: the platform's own forwards must be allowed", got)
	}
}
