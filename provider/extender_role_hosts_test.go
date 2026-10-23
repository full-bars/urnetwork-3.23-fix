package main

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/connect/extender"
)

// the allow list is the forward authorization: the server refuses every
// destination when it is empty, so parsing has to be exact
func TestExtenderAllowedHostsFromEnv(t *testing.T) {
	got := extenderAllowedHostsFromEnv(func(key string) string {
		if key == "URNETWORK_EXTENDER_ALLOWED_HOSTS" {
			return " Ads.Example.com , ,*.example.org , "
		}
		return ""
	})
	want := []string{"ads.example.com", "*.example.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("allowed hosts = %v, want %v", got, want)
	}
	if empty := extenderAllowedHostsFromEnv(func(string) string { return "" }); len(empty) != 0 {
		t.Fatalf("unset env produced %v", empty)
	}
}

// an unconfigured role refuses forwards, and a named host is allowed: this is
// the difference between a role that serves nothing and one that serves
func TestExtenderServerForwardAuthorization(t *testing.T) {
	settings := extender.DefaultExtenderSettings()
	closed := extender.NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	if closed.IsAllowedHost("ads.example.com") {
		t.Fatal("an empty allow list admitted a forward destination")
	}
	open := extender.NewExtenderServer(context.Background(), nil, []string{"ads.example.com", "*.example.org"}, nil, nil, settings)
	if !open.IsAllowedHost("ads.example.com") {
		t.Fatal("a configured destination was refused")
	}
	if !open.IsAllowedHost("cdn.example.org") {
		t.Fatal("a configured wildcard did not admit its subdomain")
	}
	if open.IsAllowedHost("other.example") {
		t.Fatal("an unlisted destination was admitted")
	}
}
