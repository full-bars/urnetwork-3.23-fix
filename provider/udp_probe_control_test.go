package main

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestUDPProbeControl(t *testing.T) {
	state := newControlState()
	if !udpProbeEnabled(state) || needsRestart("udp_probe") {
		t.Fatal("default must be enabled and live")
	}
	for _, value := range []string{"off", "on", "OFF"} {
		if err := validateControlValue("udp_probe", value); err != nil {
			t.Fatal(err)
		}
		if err := state.set("udp_probe", value); err != nil {
			t.Fatal(err)
		}
		if got := udpProbeEnabled(state); got != (value == "on") {
			t.Fatalf("%s: got %v", value, got)
		}
	}
	if validateControlValue("udp_probe", "invalid") == nil {
		t.Fatal("invalid value accepted")
	}
	if err := state.clear("udp_probe"); err != nil || !udpProbeEnabled(state) {
		t.Fatal("clear must restore default")
	}
}

func TestUDPProbeDisabledSkipsHooks(t *testing.T) {
	old, had := globalControlState.get("udp_probe")
	t.Cleanup(func() {
		if had {
			_ = globalControlState.set("udp_probe", old)
		} else {
			_ = globalControlState.clear("udp_probe")
		}
	})
	if err := globalControlState.set("udp_probe", "off"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := func(context.Context, *proxyUDPCheckStore, []*connect.ProxySettings, time.Time, time.Duration, time.Duration, float64, bool) {
		calls++
	}
	runPaidUDPProbeTickWith(context.Background(), run)
	runURLUDPProbeTickWith(context.Background(), run)
	if calls != 0 {
		t.Fatalf("disabled hooks ran %d times", calls)
	}
}
