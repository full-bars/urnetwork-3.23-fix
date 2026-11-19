package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExtenderCarrierConcurrentClearDuringRestart(t *testing.T) {
	withTempHome(t)
	oldState, oldManager := globalControlState, globalExtenderManager
	state := newControlState()
	if err := state.set("extender_udp443", "off"); err != nil {
		t.Fatal(err)
	}
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
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
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
		unblock()
		m.stop("test complete")
		globalExtenderRole.Store(nil)
		globalControlState, globalExtenderManager = oldState, oldManager
	})
	m.Reconcile("test")
	if m.Role() == nil {
		t.Fatal("role not started")
	}
	first, second := make(chan controlResponse, 1), make(chan controlResponse, 1)
	go func() {
		first <- handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp4053", Value: "off"})
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("restart did not enter")
	}
	go func() { second <- handleControlRequest(state, controlRequest{Cmd: "clear", Key: "extender_udp443"}) }()
	deadline := time.After(15 * time.Second)
	for {
		if _, ok := state.get("extender_udp443"); !ok {
			break
		}
		select {
		case <-deadline:
			unblock()
			t.Fatal("clear could not persist during restart")
		default:
			runtime.Gosched()
		}
	}
	unblock()
	for _, ch := range []<-chan controlResponse{first, second} {
		select {
		case res := <-ch:
			if !res.OK {
				t.Fatalf("control: %+v", res)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("control timed out")
		}
	}
	role := m.Role()
	if role == nil || role.config.disabledCarriers["udp 443"] || !role.config.disabledCarriers["dns 4053"] {
		t.Fatal("role did not use latest settings")
	}
}

func TestExtenderCarrierRestartEffectiveAndPersistFailure(t *testing.T) {
	home := withTempHome(t)
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
	t.Cleanup(func() {
		m.stop("test complete")
		globalExtenderRole.Store(nil)
		globalControlState, globalExtenderManager = oldState, oldManager
	})
	m.Reconcile("test")
	if m.Role() == nil {
		t.Fatal("role not started")
	}
	for _, key := range []string{"extender_udp443", "extender_udp4053", "extender_udp53"} {
		for _, step := range []struct {
			cmd, value string
			restart    bool
		}{
			{"set", "on", false}, {"clear", "", false}, {"set", "off", true},
			{"set", "OFF", false}, {"clear", "", true}, {"clear", "", false},
		} {
			before := m.Role()
			res := handleControlRequest(state, controlRequest{Cmd: step.cmd, Key: key, Value: step.value})
			if !res.OK || res.NeedsRestart {
				t.Fatalf("%s %s %s: %+v", key, step.cmd, step.value, res)
			}
			if got := m.Role() != before; got != step.restart {
				t.Fatalf("%s %s %s restart=%v want %v", key, step.cmd, step.value, got, step.restart)
			}
		}
	}
	// A regular file in place of the state directory fails for any user, including root.
	if err := os.RemoveAll(filepath.Join(home, ".urnetwork")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".urnetwork"), []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	// A replacement can capture a temporary value while a write is in progress.
	if err := state.set("extender_udp443", "off"); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartCarriers(); err != nil {
		t.Fatal(err)
	}
	temporaryRole := m.Role()
	if err := state.clear("extender_udp443"); err != nil {
		t.Fatal(err)
	}
	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if res.OK || m.Role() == temporaryRole || m.Role().config.disabledCarriers["udp 443"] {
		t.Fatal("failed write did not restore the role after a concurrent replacement")
	}
	before := m.Role()
	res = handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if res.OK || m.Role() != before || m.Role().config.disabledCarriers["udp 443"] {
		t.Fatalf("failed set must restore the role: %+v", res)
	}
	if _, ok := state.get("extender_udp443"); ok {
		t.Fatal("failed persist did not roll back")
	}
	if err := state.set("extender_udp443", "off"); err != nil {
		t.Fatal(err)
	}
	m.RestartCarriers()
	before = m.Role()
	res = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "extender_udp443"})
	if res.OK || m.Role() != before || !m.Role().config.disabledCarriers["udp 443"] {
		t.Fatalf("failed clear must restore the role: %+v", res)
	}
	if value, ok := state.get("extender_udp443"); !ok || value != "off" {
		t.Fatal("failed clear did not roll back")
	}
}

func TestExtenderCarrierLiveApplyFailure(t *testing.T) {
	withTempHome(t)
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
	t.Cleanup(func() {
		m.stop("test complete")
		globalExtenderRole.Store(nil)
		globalControlState, globalExtenderManager = oldState, oldManager
	})
	m.Reconcile("test")
	if m.Role() == nil {
		t.Fatal("role not started")
	}
	m.startRole = func(context.Context, extenderRoleConfig) (*extenderRole, error) {
		return nil, errors.New("injected start failure")
	}
	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if res.OK || !strings.Contains(res.Error, "persisted") || !strings.Contains(res.Error, "injected start failure") {
		t.Fatalf("expected live apply error: %+v", res)
	}
	if value, ok := state.get("extender_udp443"); !ok || value != "off" {
		t.Fatal("setting was not retained")
	}
	if m.Role() != nil {
		t.Fatal("role should be absent after failed start")
	}
}
