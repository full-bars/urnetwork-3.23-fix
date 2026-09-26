package main

import (
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
)

func startupSettings(addrs ...string) []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, &connect.ProxySettings{Address: a})
	}
	return out
}

func keysOf(ps []*connect.ProxySettings) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Key())
	}
	return out
}

// At startup nothing is running, so the trim cap used to be applied AFTER the
// provider launched the entire desired list ("shed 2127" one second after a
// restart), a burst of thousands of connections exactly when a box is most
// likely to run out of memory. With a cap the provider must launch only the
// best `cap` proxies and hold the rest until the cap is raised.
func TestStartupTrimSelection(t *testing.T) {
	all := startupSettings("a:1", "b:1", "c:1", "d:1", "e:1")
	state := map[string]ProxyEntry{
		"a:1": {Health: "up"},
		"b:1": {Health: "dead"},
		"c:1": {Health: "up"},
		"d:1": {Health: "offline"},
		"e:1": {Health: "up"},
	}
	noGrade := func(string) (float64, bool) { return 0, false }
	noEarnings := func(string) float64 { return 0 }

	t.Run("no cap or a cap that covers the list launches everything", func(t *testing.T) {
		for _, cap := range []int{0, -1, 5, 6, 1000} {
			launch, held := startupTrimSelection(all, cap, state, noGrade, noEarnings)
			if len(launch) != 5 || len(held) != 0 {
				t.Fatalf("cap %d: launch=%d held=%d, want 5/0", cap, len(launch), len(held))
			}
		}
	})

	t.Run("holds the dead and offline proxies first, keeps launch order", func(t *testing.T) {
		launch, held := startupTrimSelection(all, 3, state, noGrade, noEarnings)
		if got, want := keysOf(launch), []string{"a:1", "c:1", "e:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("launch = %v, want %v (original order preserved)", got, want)
		}
		if got, want := keysOf(held), []string{"b:1", "d:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("held = %v, want %v", got, want)
		}
	})

	t.Run("a proven earner is never held while idle proxies of the same health remain", func(t *testing.T) {
		// Same ranking as a live trim: health first, then earners over idle.
		// All up; only b has decayed earnings from before the restart.
		allUp := map[string]ProxyEntry{}
		for _, a := range []string{"a:1", "b:1", "c:1", "d:1", "e:1"} {
			allUp[a] = ProxyEntry{Health: "up"}
		}
		earnings := func(k string) float64 {
			if k == "b:1" {
				return 5 << 30
			}
			return 0
		}
		launch, held := startupTrimSelection(all, 3, allUp, noGrade, earnings)
		// Idle ties break on address, so the first two idle addresses are held.
		if got, want := keysOf(held), []string{"a:1", "c:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("held = %v, want %v (earner b:1 kept)", got, want)
		}
		if got, want := keysOf(launch), []string{"b:1", "d:1", "e:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("launch = %v, want %v", got, want)
		}
	})

	t.Run("unhealthy proxies are held before earners, as in a live trim", func(t *testing.T) {
		earnings := func(k string) float64 {
			if k == "b:1" { // dead but had earnings
				return 5 << 30
			}
			return 0
		}
		_, held := startupTrimSelection(all, 3, state, noGrade, earnings)
		if got, want := keysOf(held), []string{"b:1", "d:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("held = %v, want the dead and offline proxies %v", got, want)
		}
	})

	t.Run("worse grades are held before better ones among equals", func(t *testing.T) {
		flat := map[string]ProxyEntry{}
		for _, a := range []string{"a:1", "b:1", "c:1", "d:1", "e:1"} {
			flat[a] = ProxyEntry{Health: "up"}
		}
		grade := func(k string) (float64, bool) {
			return map[string]float64{"a:1": 0.9, "b:1": 0.2, "c:1": 0.8, "d:1": 0.3, "e:1": 0.7}[k], true
		}
		_, held := startupTrimSelection(all, 3, flat, grade, noEarnings)
		if got, want := keysOf(held), []string{"b:1", "d:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("held = %v, want the two lowest grades %v", got, want)
		}
	})

	t.Run("deterministic across repeated calls", func(t *testing.T) {
		l1, h1 := startupTrimSelection(all, 2, state, noGrade, noEarnings)
		for i := 0; i < 20; i++ {
			l2, h2 := startupTrimSelection(all, 2, state, noGrade, noEarnings)
			if !reflect.DeepEqual(keysOf(l1), keysOf(l2)) || !reflect.DeepEqual(keysOf(h1), keysOf(h2)) {
				t.Fatalf("run %d differs: %v/%v vs %v/%v", i, keysOf(l1), keysOf(h1), keysOf(l2), keysOf(h2))
			}
		}
	})
}
