package main

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func testH3Proxies(n int) []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, n)
	for i := range out {
		out[i] = &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1), Auth: &proxy.Auth{User: "u", Password: "p"}}
	}
	return out
}

func gradedState(candidates []*connect.ProxySettings, scores map[int]float64) *ProxyState {
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	for i, c := range candidates {
		if score, ok := scores[i]; ok {
			state.Proxies[c.Key()] = ProxyEntry{Graded: true, Score: score}
		}
	}
	return state
}

func TestRankH3ProxiesPrefersGradedThenScore(t *testing.T) {
	candidates := testH3Proxies(6)
	state := gradedState(candidates, map[int]float64{4: 0.9, 2: 0.8, 5: 0.1})
	got := rankH3Proxies(2, nil, candidates, state)
	if len(got) != 2 || !got[candidates[4].Key()] || !got[candidates[2].Key()] {
		t.Fatalf("top 2 = %v, want the two best graded proxies", got)
	}
	// ungraded ones fill when there are not enough graded
	got = rankH3Proxies(4, nil, candidates, state)
	if len(got) != 4 || !got[candidates[5].Key()] {
		t.Fatalf("top 4 = %v, want three graded plus one ungraded", got)
	}
	if got := rankH3Proxies(0, nil, candidates, state); len(got) != 0 {
		t.Fatalf("a count of zero ranks nothing, got %v", got)
	}
	if got := rankH3Proxies(100, nil, candidates, state); len(got) != 6 {
		t.Fatalf("a count above the pool ranks all %d, got %d", 6, len(got))
	}
}

// Members stay while they remain candidates, so drifting grades do not
// reconnect a working identity.
func TestRankH3ProxiesIsSticky(t *testing.T) {
	candidates := testH3Proxies(5)
	state := gradedState(candidates, map[int]float64{0: 0.9, 1: 0.8, 2: 0.1, 3: 0.95})
	previous := map[string]bool{candidates[0].Key(): true, candidates[2].Key(): true}
	got := rankH3Proxies(2, previous, candidates, state)
	if !sameKeySet(got, previous) {
		t.Fatalf("a better proxy displaced a working member: %v", got)
	}
	// a member that left the pool is replaced by the best remaining
	got = rankH3Proxies(2, previous, []*connect.ProxySettings{candidates[0], candidates[1], candidates[3]}, state)
	if !got[candidates[0].Key()] || !got[candidates[3].Key()] || len(got) != 2 {
		t.Fatalf("vacancy not filled by the best candidate: %v", got)
	}
}

func TestH3ProxyEligibleByMode(t *testing.T) {
	t.Cleanup(func() { h3RankingValue.Store(nil) })
	candidates := testH3Proxies(3)
	key := candidates[0].Key()
	for _, setting := range []h3Setting{{Mode: h3ModeOff}, {Mode: h3ModeDirect}, {Mode: h3ModeOn}} {
		if h3ProxyEligible(setting, key) {
			t.Fatalf("%v must never admit a proxy", setting)
		}
	}
	if !h3ProxyEligible(h3Setting{Mode: h3ModeAll}, key) {
		t.Fatal("all admits every proxy")
	}
	top := h3Setting{Mode: h3ModeTop, Cap: 1}
	if h3ProxyEligible(top, key) {
		t.Fatal("a count admits nothing before the ranking exists")
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{key: true}})
	if !h3ProxyEligible(top, key) || h3ProxyEligible(top, candidates[1].Key()) {
		t.Fatal("a count admits exactly the ranked proxies")
	}
	if h3ProxyEligible(h3Setting{Mode: h3ModeTop, Cap: 2}, key) {
		t.Fatal("a ranking for another count must not be used")
	}
	// auto admits exactly its own ranking, whatever the stored cap says
	auto := h3Setting{Mode: h3ModeAuto}
	h3RankingValue.Store(&h3Ranking{cap: 1, auto: true, keys: map[string]bool{key: true}})
	if !h3ProxyEligible(auto, key) || h3ProxyEligible(auto, candidates[1].Key()) {
		t.Fatal("auto must admit exactly the auto-ranked proxies")
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{key: true}})
	if h3ProxyEligible(auto, key) {
		t.Fatal("a count ranking must not admit proxies under auto")
	}
}

func TestRelaunchListFollowsEligibility(t *testing.T) {
	t.Cleanup(func() {
		h3RankingValue.Store(nil)
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
	})
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	candidates := testH3Proxies(3)
	for _, c := range candidates {
		recordH3Launch(c.Key(), false)
	}
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeDirect}); len(got) != 0 {
		t.Fatalf("nothing changes under direct, got %v", got)
	}
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeAll}); len(got) != 3 {
		t.Fatalf("all must relaunch the three H1-only proxies, got %v", got)
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{candidates[1].Key(): true}})
	got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeTop, Cap: 1})
	if len(got) != 1 || got[0] != candidates[1].Key() {
		t.Fatalf("a count of one must relaunch only its member, got %v", got)
	}
	recordH3Launch(candidates[1].Key(), true)
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeTop, Cap: 1}); len(got) != 0 {
		t.Fatalf("already launched as eligible, got %v", got)
	}
}

// A proxied identity under all gets its QUIC relayed through the proxy, never a
// host socket; the same identity under direct gets neither.
func TestPlatformSettingsForAProxyFollowTheSetting(t *testing.T) {
	proxied := testH3Proxies(1)[0]
	state := newControlState()
	saved := globalControlState
	globalControlState = state
	t.Cleanup(func() {
		globalControlState = saved
		h3RankingValue.Store(nil)
	})
	state.set("h3", "direct")
	s := platformTransportSettingsFor(proxied, false)
	if s.H3PacketConnFactory != nil || len(s.ModePreferences) != 1 {
		t.Fatalf("direct: a proxy stays on H1 with no factory, got %v", s.ModePreferences)
	}
	state.set("h3", "all")
	s = platformTransportSettingsFor(proxied, false)
	if s.H3PacketConnFactory == nil {
		t.Fatal("all: a proxy's QUIC must go through its relay, which needs the factory")
	}
	if s.EnableH3 {
		t.Fatal("a proxy is never the beside-H1 auxiliary")
	}
	if len(s.ModePreferences) != len(connect.DefaultTransportModePreferences()) {
		t.Fatalf("all: a proxy gets the fallbacks, got %v", s.ModePreferences)
	}
	state.set("h3", "off")
	if s = platformTransportSettingsFor(proxied, false); s.H3PacketConnFactory != nil || len(s.ModePreferences) != 1 {
		t.Fatal("off: H1 only")
	}
}

// Candidates are noted under every setting, so switching to a count later ranks
// over the real set instead of an empty one.
func TestCandidatesNotedBeforeACountSetting(t *testing.T) {
	h3RankingValue.Store(nil)
	t.Cleanup(func() {
		h3RankingValue.Store(nil)
		noteH3Candidates(nil, h3Setting{Mode: h3ModeDirect})
	})
	candidates := testH3Proxies(5)
	if noteH3Candidates(candidates, h3Setting{Mode: h3ModeDirect}) {
		t.Fatal("no ranking exists under direct, so nothing changed")
	}
	if !rerankH3(h3Setting{Mode: h3ModeTop, Cap: 2}) {
		t.Fatal("switching to a count must rank the candidates noted earlier")
	}
	ranking := h3RankingValue.Load()
	if ranking == nil || len(ranking.keys) != 2 {
		t.Fatalf("ranking = %+v, want 2 members", ranking)
	}
	if !rerankH3(h3Setting{Mode: h3ModeDirect}) || h3RankingValue.Load() != nil {
		t.Fatal("leaving the count must drop the ranking")
	}
}

// A relaunch takes the proxy's entry out of the running set itself. A proxy
// goroutine never does, so a cancel alone leaves the reload pass counting it as
// running and nothing starts it again.
func TestDetachProxyForRelaunchRemovesTheRunningEntry(t *testing.T) {
	cancelled := false
	r := &ProxyReloader{
		cancelMapMu: &sync.Mutex{},
		cancelMap:   map[string]context.CancelFunc{"p": func() { cancelled = true }},
		runningAuth: map[string]*connect.ProxySettings{"p": {}},
	}
	if !r.detachProxyForRelaunch("p") {
		t.Fatal("a running proxy must be detached")
	}
	if !cancelled {
		t.Fatal("the proxy was not cancelled")
	}
	if _, still := r.cancelMap["p"]; still {
		t.Fatal("the entry stayed in the running set, so the reload pass would never restart it")
	}
	if _, still := r.runningAuth["p"]; still {
		t.Fatal("the launch credentials stayed recorded")
	}
	if r.detachProxyForRelaunch("p") {
		t.Fatal("a proxy that is not running must report false")
	}
}
