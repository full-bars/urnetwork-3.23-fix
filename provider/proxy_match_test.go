package main

import "testing"

func TestHostOfAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dc.decodo.com:8001", "dc.decodo.com"},
		{"191.101.31.7:4444", "191.101.31.7"},
		{"noport.example.com", "noport.example.com"},
		{"[2001:db8::1]:1080", "2001:db8::1"},
	}
	for _, c := range cases {
		if got := hostOfAddress(c.in); got != c.want {
			t.Errorf("hostOfAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchProxyHost(t *testing.T) {
	cases := []struct {
		pattern, address string
		want             bool
	}{
		{"dc.decodo.com", "dc.decodo.com:8001", true},
		{"DECODO", "dc.decodo.com:8001", true}, // case-insensitive
		{"decodo", "gate.smartproxy.com:7000", false},
		{"191.3.", "191.3.44.7:1080", true},                 // IP prefix
		{"8001", "dc.decodo.com:8001", false},               // never match port
		{"user", "host.com:1080:user:pass", false},          // never match credentials
		{"", "dc.decodo.com:8001", false},                   // empty pattern matches nothing
		{"decodo", "dc.decodo.com:8001:alice:secret", true}, // credentialed form still matches host
	}
	for _, c := range cases {
		if got := matchProxyHost(c.pattern, c.address); got != c.want {
			t.Errorf("matchProxyHost(%q, %q) = %v, want %v", c.pattern, c.address, got, c.want)
		}
	}
}

func TestCollectMatchingProxies(t *testing.T) {
	servers := map[string]string{
		"dc.decodo.com:8001:alice:secret": "",
		"gate.smartproxy.com:7000":        "",
	}
	stateProxies := map[string]ProxyEntry{
		"dc.decodo.com:8002": {Source: "file"},
		"dc.decodo.com:8003": {Source: "url"},
		"1.2.3.4:1080":       {Source: "url"},
	}
	urlCache := map[string]ProxyURLEntry{
		"dc.decodo.com:8003": {},
		"1.2.3.4:1080":       {},
		"dc.decodo.com:9999": {}, // cached but not in state (not yet launched)
	}

	addrsBySource, display := collectMatchingProxies("dc.decodo.com", servers, stateProxies, "/etc/proxies.txt", urlCache)

	if got := addrsBySource["internal"]; len(got) != 1 || got[0] != "dc.decodo.com:8001" {
		t.Errorf("internal = %v, want [dc.decodo.com:8001]", got)
	}
	if got := addrsBySource["file"]; len(got) != 1 || got[0] != "dc.decodo.com:8002" {
		t.Errorf("file = %v, want [dc.decodo.com:8002]", got)
	}
	// url matches come from both state (running) and cache (not yet launched), deduped
	urlGot := map[string]bool{}
	for _, a := range addrsBySource["url"] {
		urlGot[a] = true
	}
	if len(urlGot) != 2 || !urlGot["dc.decodo.com:8003"] || !urlGot["dc.decodo.com:9999"] {
		t.Errorf("url = %v, want dc.decodo.com:8003 and dc.decodo.com:9999", addrsBySource["url"])
	}
	if len(display) != 4 {
		t.Errorf("display = %v, want 4 entries", display)
	}
}

func TestCollectMatchingProxiesNoState(t *testing.T) {
	// Provider never ran: state is empty, but proxy.json + URL cache still work.
	servers := map[string]string{"dc.decodo.com:8001": ""}
	urlCache := map[string]ProxyURLEntry{"dc.decodo.com:8002": {}}

	addrsBySource, display := collectMatchingProxies("decodo", servers, nil, "", urlCache)

	if len(addrsBySource["internal"]) != 1 || len(addrsBySource["url"]) != 1 {
		t.Errorf("addrsBySource = %v, want 1 internal + 1 url", addrsBySource)
	}
	if len(display) != 2 {
		t.Errorf("display = %v, want 2 entries", display)
	}
}

func TestRemoveExcludePattern(t *testing.T) {
	state := &ProxyURLState{ExcludePatterns: []string{"dc.decodo.com", "191.3."}}
	if !removeExcludePattern(state, "DC.DECODO.COM") {
		t.Fatal("case-insensitive removal should return true")
	}
	if removeExcludePattern(state, "dc.decodo.com") {
		t.Fatal("second removal should return false")
	}
	if len(state.ExcludePatterns) != 1 || state.ExcludePatterns[0] != "191.3." {
		t.Fatalf("ExcludePatterns = %v, want [191.3.]", state.ExcludePatterns)
	}
}
