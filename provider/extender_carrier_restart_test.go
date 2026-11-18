package main

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestExtenderCarrierControlRestartsOutsideTransaction(t *testing.T) {
	withTempHome(t)
	t.Setenv(extenderDns53Env, "on")
	oldState, oldManager := globalControlState, globalExtenderManager
	state := newControlState()
	globalControlState = state
	m := newExtenderManager()
	globalExtenderManager = m
	m.environ = func(string) string { return "" }
	m.directEnabled = func() bool { return true }
	m.ctx = context.Background()
	seams := testExtenderRoleConfig(t)
	m.newConfig = func() extenderRoleConfig {
		cfg := defaultExtenderRoleConfig()
		cfg.keyPath, cfg.listen, cfg.listenPacket = seams.keyPath, seams.listen, seams.listenPacket
		return cfg
	}
	entered, release := make(chan struct{}), make(chan struct{})
	starts := 0
	m.startRole = func(ctx context.Context, cfg extenderRoleConfig) (*extenderRole, error) {
		starts++
		if starts == 2 {
			close(entered)
			<-release
		}
		return startExtenderRole(ctx, cfg)
	}
	t.Cleanup(func() {
		m.stop("test complete")
		globalExtenderRole.Store(nil)
		globalControlState = oldState
		globalExtenderManager = oldManager
	})
	m.Reconcile("test")
	firstRole := m.Role()
	if firstRole == nil {
		t.Fatal("role not started")
	}
	waitExtenderRoleReady(t, firstRole)
	first := make(chan controlResponse, 1)
	go func() {
		first <- handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("restart did not reach start")
	}
	second := make(chan controlResponse, 1)
	go func() {
		second <- handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp4053", Value: "off"})
	}()
	deadline := time.After(15 * time.Second)
	for {
		if value, ok := state.get("extender_udp4053"); ok && value == "off" {
			break
		}
		select {
		case <-deadline:
			close(release)
			t.Fatal("second change could not persist while role restart blocked")
		default:
			runtime.Gosched()
		}
	}
	close(release)
	for _, ch := range []<-chan controlResponse{first, second} {
		select {
		case res := <-ch:
			if !res.OK || res.NeedsRestart {
				t.Fatalf("set: %+v", res)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("control change did not finish")
		}
	}
	role := m.Role()
	if role == nil || role == firstRole {
		t.Fatal("role not replaced")
	}
	waitExtenderRoleReady(t, role)
	if !role.config.disabledCarriers["udp 443"] || !role.config.disabledCarriers["dns 4053"] {
		t.Fatalf("latest settings lost: %+v", role.config.disabledCarriers)
	}
	if res := handleControlRequest(state, controlRequest{Cmd: "clear", Key: "extender_udp443"}); !res.OK || res.NeedsRestart {
		t.Fatalf("clear: %+v", res)
	}
	if m.Role().config.disabledCarriers["udp 443"] {
		t.Fatal("clear did not restore default")
	}
}
