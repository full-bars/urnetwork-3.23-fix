//go:build linux

package main

import (
	"context"
	"runtime/debug"
	"testing"
)

// TestSetGogcOffNeverDisablesCollection: "off" is the clear value for every
// tuning key. A set carrying it in any casing must leave garbage collection
// on; only "disabled" turns it off.
func TestSetGogcOffNeverDisablesCollection(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	orig := debug.SetGCPercent(50)
	t.Cleanup(func() { debug.SetGCPercent(orig) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	for _, value := range []string{"off", "OFF", "Off"} {
		debug.SetGCPercent(50)
		resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "gogc", Value: value})
		if err != nil {
			t.Fatalf("dial set gogc=%s: %v", value, err)
		}
		if !resp.OK {
			t.Fatalf("set gogc=%s: %v", value, resp.Error)
		}
		if got := debug.SetGCPercent(50); got != 100 {
			t.Errorf("after set gogc=%s: GC percent = %d, want 100 (the default)", value, got)
		}
	}

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "gogc", Value: "disabled"})
	if err != nil || !resp.OK {
		t.Fatalf("set gogc=disabled: resp=%+v err=%v", resp, err)
	}
	if got := debug.SetGCPercent(50); got != -1 {
		t.Errorf("after set gogc=disabled: GC percent = %d, want -1", got)
	}
}
