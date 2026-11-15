package main

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func stopManagedPprof(t *testing.T) {
	t.Helper()
	pprofApplyMu.Lock()
	pprofRetrySeq++
	pprofApplyMu.Unlock()
	_ = managedPprof.stop()
}

func pprofGet(t *testing.T, addr string) (int, error) {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func TestValidPprofValue(t *testing.T) {
	for _, value := range []string{"on", "off", " ON ", "127.0.0.1:6060", "[::1]:6060", "127.0.0.1:0"} {
		if !validPprofValue(value) {
			t.Fatalf("%q should be valid", value)
		}
	}
	for _, value := range []string{"", "yes", "0.0.0.0:6060", "10.0.0.1:6060", "localhost:6060", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:x"} {
		if validPprofValue(value) {
			t.Fatalf("%q should be invalid", value)
		}
	}
}

func TestPprofControlKeyIsRegistered(t *testing.T) {
	if !controlKeys["pprof"] || !liveEffectKeys["pprof"] {
		t.Fatal("pprof must be a persisted key with a live effect")
	}
	if liveDefaults["pprof"] != "on" {
		t.Fatalf("the default is %q, want on", liveDefaults["pprof"])
	}
	for _, value := range []string{"on", "off", "127.0.0.1:6060"} {
		if err := validateControlValue("pprof", value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	if validateControlValue("pprof", "0.0.0.0:6060") == nil {
		t.Fatal("a non-loopback address was accepted")
	}
}

func TestApplyPprofModeOnOffAndMove(t *testing.T) {
	t.Setenv("URNETWORK_PPROF", "")
	stopManagedPprof(t)
	t.Cleanup(func() { stopManagedPprof(t) })

	if err := applyPprofMode("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	first := pprofStatusAddr()
	if first == "" {
		t.Fatal("no address after on")
	}
	if status, err := pprofGet(t, first); err != nil || status != http.StatusOK {
		t.Fatalf("GET %s = %d, %v", first, status, err)
	}
	if !strings.HasPrefix(first, "127.0.0.1:") {
		t.Fatalf("bound to %s, want loopback", first)
	}
	// the same wanted address is a no-op
	if err := applyPprofMode("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	// off stops it
	if err := applyPprofMode("off"); err != nil {
		t.Fatal(err)
	}
	if pprofStatusAddr() != "" {
		t.Fatal("listener still bound after off")
	}
	if _, err := pprofGet(t, first); err == nil {
		t.Fatal("the old address still answers after off")
	}
	// back on
	if err := applyPprofMode("on"); err != nil && !strings.Contains(err.Error(), "in use") {
		// the default port can be busy on a developer machine; the fallback handles that
		t.Fatal(err)
	}
	if pprofStatusAddr() == "" {
		t.Fatal("no address after turning it back on")
	}
	if err := applyPprofMode("bogus"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
}

func TestApplyPprofModeEnvOverrideWins(t *testing.T) {
	t.Setenv("URNETWORK_PPROF", "127.0.0.1:0")
	stopManagedPprof(t)
	if err := applyPprofMode("on"); err != nil {
		t.Fatal(err)
	}
	if pprofStatusAddr() != "" {
		t.Fatal("the managed listener started although URNETWORK_PPROF decides the address")
	}
}

func TestApplyPprofModeMovesOntoItsPortWhenTheHolderExits(t *testing.T) {
	t.Setenv("URNETWORK_PPROF", "")
	stopManagedPprof(t)
	t.Cleanup(func() { stopManagedPprof(t) })
	oldEvery, oldFor := pprofRetryEvery, pprofRetryFor
	pprofRetryEvery, pprofRetryFor = 20*time.Millisecond, 10*time.Second
	t.Cleanup(func() { pprofRetryEvery, pprofRetryFor = oldEvery, oldFor })

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := holder.Addr().String()
	if err := applyPprofMode(want); err != nil {
		holder.Close()
		t.Fatal(err)
	}
	if got := pprofStatusAddr(); got == want || got == "" {
		holder.Close()
		t.Fatalf("bound %q while %s is held, want a fallback port", got, want)
	}
	if _, fellBack := managedPprof.usingFallback(); !fellBack {
		holder.Close()
		t.Fatal("the fallback was not reported")
	}
	// the holder exits, like a hotswap parent finishing its drain
	holder.Close()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if pprofStatusAddr() == want {
			if _, fellBack := managedPprof.usingFallback(); !fellBack {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the listener did not move to %s, it is on %q", want, pprofStatusAddr())
}
