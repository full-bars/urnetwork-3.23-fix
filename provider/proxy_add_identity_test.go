package main

import (
	"reflect"
	"testing"
)

// Adding an account at a shared gateway must not touch the other accounts
// there: proxy identity is address+user, so a different user is a different
// proxy, not a rotation.
func TestPlanInternalAdd_DifferentUserAtSharedGatewayIsNotStale(t *testing.T) {
	cfg := &ProxyConfig{Servers: map[string]string{"gw.example:1080:u1:p1": ""}}
	keep, stale := planInternalAdd(cfg, "gw.example:1080:u2:p2", "gw.example:1080", "u2", "p2")
	if keep || len(stale) != 0 {
		t.Fatalf("a second account at the gateway must be left alone, got keep=%v stale=%v", keep, stale)
	}
}

// Same identity (address+user) with a new password IS a rotation.
func TestPlanInternalAdd_SameUserNewPasswordRotates(t *testing.T) {
	cfg := &ProxyConfig{Servers: map[string]string{"gw.example:1080:u1:old": ""}}
	keep, stale := planInternalAdd(cfg, "gw.example:1080:u1:new", "gw.example:1080", "u1", "new")
	if keep || !reflect.DeepEqual(stale, []string{"gw.example:1080:u1:old"}) {
		t.Fatalf("password change on one identity must purge the old entry, got keep=%v stale=%v", keep, stale)
	}
}

// Identical credentials in an alternate representation (Auths table) are not a
// rotation; and a stale duplicate of the SAME identity is still purged, in a
// stable order.
func TestPlanInternalAdd_EquivalentEntryKeptAndDuplicatesPurgedDeterministically(t *testing.T) {
	cfg := &ProxyConfig{
		Servers: map[string]string{
			"gw.example:1080":        "ref", // creds via Auths: u1/p1, identical to the add
			"gw.example:1080:u1:old": "",
			"gw.example:1080:u1:zzz": "",
			"gw.example:1080:u2:p2":  "", // other account: untouched
		},
		Auths: map[string]*ProxyAuth{"ref": {User: "u1", Password: "p1"}},
	}
	keep, stale := planInternalAdd(cfg, "gw.example:1080:u1:p1", "gw.example:1080", "u1", "p1")
	want := []string{"gw.example:1080:u1:old", "gw.example:1080:u1:zzz"}
	if !keep || !reflect.DeepEqual(stale, want) {
		t.Fatalf("keep=%v stale=%v, want keep=true stale=%v", keep, stale, want)
	}
}
