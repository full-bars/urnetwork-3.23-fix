package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyWarmthTier_String(t *testing.T) {
	tests := []struct {
		tier ProxyWarmthTier
		want string
	}{
		{WarmthValid, "valid"},
		{WarmthRenewable, "renewable"},
		{WarmthCold, "cold"},
		{ProxyWarmthTier(99), "cold"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("ProxyWarmthTier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestProxyLaunchStagger(t *testing.T) {
	tests := []struct {
		name         string
		tier         ProxyWarmthTier
		isURLSourced bool
		want         time.Duration
	}{
		{"WarmthValid file", WarmthValid, false, WarmValidStagger},
		{"WarmthValid url", WarmthValid, true, WarmValidStagger},
		{"WarmthRenewable file", WarmthRenewable, false, WarmRenewableStagger},
		{"WarmthRenewable url", WarmthRenewable, true, WarmRenewableStagger},
		{"WarmthCold file", WarmthCold, false, ColdFileStagger},
		{"WarmthCold url", WarmthCold, true, ColdURLStagger},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proxyLaunchStagger(tt.tier, tt.isURLSourced)
			if got != tt.want {
				t.Errorf("proxyLaunchStagger(%v, %v) = %v, want %v", tt.tier, tt.isURLSourced, got, tt.want)
			}
		})
	}
}

func TestCurrentProviderNetworkID(t *testing.T) {
	t.Run("no jwt file returns empty", func(t *testing.T) {
		_, restore := withHome(t)
		defer restore()

		if got := currentProviderNetworkID(); got != "" {
			t.Errorf("currentProviderNetworkID() = %q, want empty", got)
		}
	})

	t.Run("jwt file with network_id returns network_id", func(t *testing.T) {
		home, restore := withHome(t)
		defer restore()

		writeAccountJWT(t, home, map[string]interface{}{
			"network_id": "net-xyz-123",
		})

		if got := currentProviderNetworkID(); got != "net-xyz-123" {
			t.Errorf("currentProviderNetworkID() = %q, want net-xyz-123", got)
		}
	})
}

func TestEvaluateProxyWarmth(t *testing.T) {
	home := t.TempDir()
	storePath := filepath.Join(home, ".client_jwts.json")
	restoreStore := withGlobalStore(t, storePath)
	defer restoreStore()

	// Clean env on exit
	origHotRestart, hadHotRestart := os.LookupEnv("URNETWORK_HOT_RESTART")
	defer func() {
		if hadHotRestart {
			_ = os.Setenv("URNETWORK_HOT_RESTART", origHotRestart)
		} else {
			_ = os.Unsetenv("URNETWORK_HOT_RESTART")
		}
	}()
	_ = os.Unsetenv("URNETWORK_HOT_RESTART")

	validExp := float64(time.Now().Add(time.Hour).Unix())
	expiredExp := float64(time.Now().Add(-time.Hour).Unix())

	validJWTWithClientID := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        validExp,
		"network_id": "net-abc",
	})
	expiredJWTWithClientID := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        expiredExp,
		"network_id": "net-abc",
	})
	validJWTWithoutClientID := createFakeJWTWithClaims(map[string]interface{}{
		"exp":        validExp,
		"network_id": "net-abc",
	})
	expiredJWTWithoutClientID := createFakeJWTWithClaims(map[string]interface{}{
		"exp":        expiredExp,
		"network_id": "net-abc",
	})

	// Preload store entries
	_ = globalClientJWTStore.Put("proxy-valid", clientJWTEntry{
		ByClientJWT: validJWTWithClientID,
		ClientID:    testClientId,
		NetworkID:   "net-abc",
		MintedAt:    time.Now(),
	})
	_ = globalClientJWTStore.Put("proxy-renewable", clientJWTEntry{
		ByClientJWT: expiredJWTWithClientID,
		ClientID:    testClientId,
		NetworkID:   "net-abc",
		MintedAt:    time.Now().Add(-2 * time.Hour),
	})
	_ = globalClientJWTStore.Put("proxy-mismatched-network", clientJWTEntry{
		ByClientJWT: validJWTWithClientID,
		ClientID:    testClientId,
		NetworkID:   "net-other",
		MintedAt:    time.Now(),
	})
	_ = globalClientJWTStore.Put("proxy-empty-fields", clientJWTEntry{
		ByClientJWT: "",
		ClientID:    "",
		NetworkID:   "net-abc",
	})
	_ = globalClientJWTStore.Put("proxy-invalid-clientid", clientJWTEntry{
		ByClientJWT: expiredJWTWithClientID,
		ClientID:    "not-a-valid-uuid",
		NetworkID:   "net-abc",
		MintedAt:    time.Now().Add(-2 * time.Hour),
	})
	_ = globalClientJWTStore.Put("proxy-no-clientid-in-jwt", clientJWTEntry{
		ByClientJWT: validJWTWithoutClientID,
		ClientID:    testClientId,
		NetworkID:   "net-abc",
		MintedAt:    time.Now(),
	})
	_ = globalClientJWTStore.Put("proxy-expired-no-clientid-in-jwt", clientJWTEntry{
		ByClientJWT: expiredJWTWithoutClientID,
		ClientID:    testClientId,
		NetworkID:   "net-abc",
		MintedAt:    time.Now().Add(-2 * time.Hour),
	})

	_ = globalClientJWTStore.Put("proxy-legacy-empty-net", clientJWTEntry{
		ByClientJWT: validJWTWithClientID,
		ClientID:    testClientId,
		NetworkID:   "", // legacy entry
		MintedAt:    time.Now(),
	})

	t.Run("valid unexpired token returns WarmthValid", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-valid", "net-abc")
		if got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthValid", got)
		}
	})

	t.Run("expired token with valid client_id returns WarmthRenewable", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-renewable", "net-abc")
		if got != WarmthRenewable {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthRenewable", got)
		}
	})

	t.Run("network mismatch returns WarmthCold", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-mismatched-network", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("missing address returns WarmthCold", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-nonexistent", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("empty fields return WarmthCold", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-empty-fields", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("invalid client ID format returns WarmthCold", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-invalid-clientid", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthCold", got)
		}
	})

	t.Run("valid jwt lacking client_id claim in payload returns WarmthRenewable (salvage via renewal)", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-no-clientid-in-jwt", "net-abc")
		if got != WarmthRenewable {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthRenewable", got)
		}
	})

	t.Run("expired jwt lacking client_id claim returns WarmthRenewable (salvage via renewal)", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-expired-no-clientid-in-jwt", "net-abc")
		if got != WarmthRenewable {
			t.Errorf("evaluateProxyWarmth() = %v, want WarmthRenewable", got)
		}
	})

	t.Run("hot restart disabled returns WarmthCold", func(t *testing.T) {
		_ = os.Setenv("URNETWORK_HOT_RESTART", "0")
		got := evaluateProxyWarmth("proxy-valid", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() with HOT_RESTART=0 = %v, want WarmthCold", got)
		}
		_ = os.Unsetenv("URNETWORK_HOT_RESTART")
	})

	t.Run("empty currentNetworkID filled in from entry.NetworkID returns WarmthValid", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-valid", "")
		if got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() with empty currentNetworkID but stored NetworkID = %v, want WarmthValid", got)
		}
	})

	t.Run("legacy entry with empty NetworkID and known currentNetworkID returns WarmthValid", func(t *testing.T) {
		got := evaluateProxyWarmth("proxy-legacy-empty-net", "net-abc")
		if got != WarmthValid {
			t.Errorf("evaluateProxyWarmth() legacy empty NetworkID = %v, want WarmthValid", got)
		}
	})

	t.Run("empty currentNetworkID and all empty entries return WarmthCold", func(t *testing.T) {
		isolatedHome := t.TempDir()
		restoreIso := withGlobalStore(t, filepath.Join(isolatedHome, "store.json"))
		defer restoreIso()

		_ = globalClientJWTStore.Put("proxy-only-legacy", clientJWTEntry{
			ByClientJWT: validJWTWithClientID,
			ClientID:    testClientId,
			NetworkID:   "", // legacy entry with no network_id anywhere in store
			MintedAt:    time.Now(),
		})

		got := evaluateProxyWarmth("proxy-only-legacy", "")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() with no network_id anywhere = %v, want WarmthCold", got)
		}
	})

	t.Run("nil globalClientJWTStore returns WarmthCold", func(t *testing.T) {
		orig := globalClientJWTStore
		globalClientJWTStore = nil
		defer func() { globalClientJWTStore = orig }()

		got := evaluateProxyWarmth("proxy-valid", "net-abc")
		if got != WarmthCold {
			t.Errorf("evaluateProxyWarmth() with nil store = %v, want WarmthCold", got)
		}
	})
}

func TestPrioritizeAndScheduleProxies(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	home := t.TempDir()
	storePath := filepath.Join(home, ".client_jwts.json")
	restoreStore := withGlobalStore(t, storePath)
	defer restoreStore()

	validExp := float64(time.Now().Add(time.Hour).Unix())
	expiredExp := float64(time.Now().Add(-time.Hour).Unix())

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        validExp,
		"network_id": "net-main",
	})
	expiredJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        expiredExp,
		"network_id": "net-main",
	})

	_ = globalClientJWTStore.Put("file-warm", clientJWTEntry{
		ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main",
	})
	_ = globalClientJWTStore.Put("file-renewable", clientJWTEntry{
		ByClientJWT: expiredJWT, ClientID: testClientId, NetworkID: "net-main",
	})
	_ = globalClientJWTStore.Put("url-warm", clientJWTEntry{
		ByClientJWT: validJWT, ClientID: testClientId, NetworkID: "net-main",
	})
	_ = globalClientJWTStore.Put("url-renewable", clientJWTEntry{
		ByClientJWT: expiredJWT, ClientID: testClientId, NetworkID: "net-main",
	})

	proxies := []*connect.ProxySettings{
		{Address: "url-cold"},
		{Address: "file-cold"},
		{Address: "url-renewable"},
		{Address: "file-renewable"},
		{Address: "url-warm"},
		{Address: "file-warm"},
	}

	sourceOf := map[string]string{
		"file-warm":      "file",
		"file-renewable": "file",
		"file-cold":      "file",
		"url-warm":       "url",
		"url-renewable":  "url",
		"url-cold":       "url",
	}

	schedules, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(proxies, sourceOf, "net-main")

	if warmCount != 2 {
		t.Errorf("warmCount = %d, want 2", warmCount)
	}
	if renewableCount != 2 {
		t.Errorf("renewableCount = %d, want 2", renewableCount)
	}
	if coldCount != 2 {
		t.Errorf("coldCount = %d, want 2", coldCount)
	}

	expectedOrder := []struct {
		addr    string
		tier    ProxyWarmthTier
		delay   time.Duration
		stagger time.Duration
	}{
		{"file-warm", WarmthValid, 0, WarmValidStagger},                                  // 0
		{"file-renewable", WarmthRenewable, 25 * time.Millisecond, WarmRenewableStagger}, // 25ms
		{"file-cold", WarmthCold, 75 * time.Millisecond, ColdFileStagger},                // 25ms + 50ms = 75ms
		{"url-warm", WarmthValid, 225 * time.Millisecond, WarmValidStagger},              // 75ms + 150ms = 225ms
		{"url-renewable", WarmthRenewable, 250 * time.Millisecond, WarmRenewableStagger}, // 225ms + 25ms = 250ms
		{"url-cold", WarmthCold, 300 * time.Millisecond, ColdURLStagger},                 // 250ms + 50ms = 300ms
	}

	if len(schedules) != len(expectedOrder) {
		t.Fatalf("len(schedules) = %d, want %d", len(schedules), len(expectedOrder))
	}

	for i, exp := range expectedOrder {
		sched := schedules[i]
		if sched.Settings.Address != exp.addr {
			t.Errorf("schedule[%d].Settings.Address = %q, want %q", i, sched.Settings.Address, exp.addr)
		}
		if sched.Tier != exp.tier {
			t.Errorf("schedule[%d] (%s) Tier = %v, want %v", i, exp.addr, sched.Tier, exp.tier)
		}
		if sched.Delay != exp.delay {
			t.Errorf("schedule[%d] (%s) Delay = %v, want %v", i, exp.addr, sched.Delay, exp.delay)
		}
		if sched.Stagger != exp.stagger {
			t.Errorf("schedule[%d] (%s) Stagger = %v, want %v", i, exp.addr, sched.Stagger, exp.stagger)
		}
	}
}

func TestPrioritizeAndScheduleProxies_StableOrder(t *testing.T) {
	home := t.TempDir()
	storePath := filepath.Join(home, ".client_jwts.json")
	restoreStore := withGlobalStore(t, storePath)
	defer restoreStore()

	// All proxies in the same source and warmth tier should preserve original order
	proxies := []*connect.ProxySettings{
		{Address: "file-cold-1"},
		{Address: "file-cold-2"},
		{Address: "file-cold-3"},
	}
	sourceOf := map[string]string{
		"file-cold-1": "file",
		"file-cold-2": "file",
		"file-cold-3": "file",
	}

	schedules, _, _, coldCount := prioritizeAndScheduleProxies(proxies, sourceOf, "net-123")
	if coldCount != 3 {
		t.Errorf("coldCount = %d, want 3", coldCount)
	}
	for i, expectedAddr := range []string{"file-cold-1", "file-cold-2", "file-cold-3"} {
		if schedules[i].Settings.Address != expectedAddr {
			t.Errorf("schedule[%d] = %s, want %s (stable sort preserved)", i, schedules[i].Settings.Address, expectedAddr)
		}
	}
}

func TestBackoffPacerWithDelay(t *testing.T) {
	ctx := context.Background()

	t.Run("zero delay returns immediately", func(t *testing.T) {
		start := time.Now()
		ok := backoffPacerWithDelay(0, 0, ctx)
		if !ok {
			t.Error("expected backoffPacerWithDelay to return true")
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Errorf("expected immediate return, elapsed %v", elapsed)
		}
	})

	t.Run("context cancellation terminates immediately", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel() // canceled upfront

		start := time.Now()
		ok := backoffPacerWithDelay(2*time.Second, 100*time.Millisecond, cancelCtx)
		if ok {
			t.Error("expected backoffPacerWithDelay to return false on canceled context")
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("expected fast exit on canceled context, elapsed %v", elapsed)
		}
	})

	t.Run("completes after delay", func(t *testing.T) {
		start := time.Now()
		ok := backoffPacerWithDelay(15*time.Millisecond, 2*time.Millisecond, ctx)
		if !ok {
			t.Error("expected backoffPacerWithDelay to return true")
		}
		elapsed := time.Since(start)
		if elapsed < 10*time.Millisecond {
			t.Errorf("expected elapsed >= 10ms, got %v", elapsed)
		}
	})

	t.Run("legacy backoffPacer wrapper behaves identically", func(t *testing.T) {
		if !backoffPacer(0, 0, time.Now(), ctx) {
			t.Error("backoffPacer(0, 0) returned false, want true")
		}

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		if backoffPacer(5, 100, time.Now(), cancelCtx) {
			t.Error("backoffPacer with canceled context returned true, want false")
		}
	})
}
