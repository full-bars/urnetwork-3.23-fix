package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestExtenderCarrierSettingsCombinations(t *testing.T) {
	withTempHome(t)
	previous := globalControlState
	globalControlState = newControlState()
	t.Cleanup(func() { globalControlState = previous; globalExtenderRole.Store(nil) })
	t.Setenv(extenderDns53Env, "on")
	keys := []string{"extender_udp443", "extender_udp4053", "extender_udp53"}
	names := []string{"udp 443", "dns 4053", "dns 53"}
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			for i, key := range keys {
				value := "off"
				if mask&(1<<i) != 0 {
					value = "on"
				}
				if err := globalControlState.set(key, value); err != nil {
					t.Fatal(err)
				}
			}
			config := defaultExtenderRoleConfig()
			seams := testExtenderRoleConfig(t)
			config.keyPath, config.listen, config.listenPacket = seams.keyPath, seams.listen, seams.listenPacket
			role, err := startExtenderRole(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer role.CloseAndWait()
			waitExtenderRoleReady(t, role)
			snapshot := role.snapshot()
			got := []bool{slices.Contains(snapshot.Carriers, connect.ExtenderCarrierQuic), slices.Contains(snapshot.DnsPorts, 4053), slices.Contains(snapshot.DnsPorts, 53)}
			advertised := []bool{slices.Contains(role.activationCarriers(), connect.ExtenderCarrierQuic), slices.Contains(role.activationDnsPorts(), 4053), slices.Contains(role.activationDnsPorts(), 53)}
			m := newExtenderManager()
			m.role = role
			reading := m.Reading()
			text := formatExtenderHealthLine(reading, time.Now())
			for i, name := range names {
				want := mask&(1<<i) != 0
				if got[i] != want || advertised[i] != want {
					t.Errorf("%s bound=%v offered=%v want %v; snapshot=%+v", name, got[i], advertised[i], want, snapshot)
				}
				if !want && !strings.Contains(text, name+" disabled by setting") {
					t.Errorf("missing disabled state: %s", text)
				}
			}
		})
	}
}

func TestExtenderCarrierSettingsControlPersistence(t *testing.T) {
	withTempHome(t)
	state := newControlState()
	for _, key := range []string{"extender_udp443", "extender_udp4053", "extender_udp53"} {
		if validateControlValue(key, "invalid") == nil {
			t.Fatalf("%s accepted invalid", key)
		}
		if res := handleControlRequest(state, controlRequest{Cmd: "set", Key: key, Value: "off"}); !res.OK {
			t.Fatalf("%s: %+v", key, res)
		}
	}
	loaded, err := loadControlState()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"extender_udp443", "extender_udp4053", "extender_udp53"} {
		if v, ok := loaded.get(key); !ok || v != "off" {
			t.Fatalf("%s = %q, %v", key, v, ok)
		}
	}
}

func TestExtenderCarrierSettingsDefaultsAndEnv(t *testing.T) {
	previous := globalControlState
	globalControlState = newControlState()
	t.Cleanup(func() { globalControlState = previous })
	t.Setenv(extenderDns53Env, "on")
	config := defaultExtenderRoleConfig()
	if !config.dnsPrivilegedPort || !slices.Contains(config.ports[443], connect.ExtenderConnectModeQuic) || !slices.Contains(config.ports[4053], connect.ExtenderConnectModeDns) {
		t.Fatalf("defaults: %+v", config)
	}
	t.Setenv(extenderDns53Env, "off")
	if defaultExtenderRoleConfig().dnsPrivilegedPort {
		t.Fatal("environment did not disable 53")
	}
}
