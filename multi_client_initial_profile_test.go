package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// A window starts its resize and enumeration workers as soon as it is built,
// so it must already hold the effective performance profile: the default from
// the settings, with the allow-direct override applied. A window built with no
// profile would size itself and set up its channels from the defaults until a
// later SetPerformanceProfile call.
func TestMultiClientWindowsStartWithTheEffectiveProfile(t *testing.T) {
	for _, allow := range []bool{false, true} {
		settings := DefaultMultiClientSettings()
		settings.DefaultPerformanceProfile = &PerformanceProfile{AllowDirect: !allow}
		settings.OverrideAllowDirect = &allow
		client := NewRemoteUserNatMultiClient(context.Background(), &testingEmptyMultiClientGenerator{}, nil, protocol.ProvideMode_Network, settings)
		if len(client.windows) == 0 {
			t.Fatal("no windows were created")
		}
		for kind, window := range client.windows {
			window.stateLock.Lock()
			profile := window.performanceProfile
			window.stateLock.Unlock()
			if profile == nil {
				t.Errorf("window %v started with no profile", kind)
				continue
			}
			if profile.AllowDirect != allow {
				t.Errorf("window %v: AllowDirect = %v, want %v", kind, profile.AllowDirect, allow)
			}
		}
		client.Close()
	}
}

// Whatever the settings hold, each window starts with the profile the parent
// uses, so the two never disagree before the first SetPerformanceProfile call.
func TestMultiClientWindowsStartWithTheParentProfile(t *testing.T) {
	for name, configure := range map[string]func(*MultiClientSettings){
		"no profile": func(s *MultiClientSettings) { s.DefaultPerformanceProfile = nil },
		"fixed size": func(s *MultiClientSettings) {
			s.DefaultPerformanceProfile = &PerformanceProfile{WindowType: WindowTypeQuality, WindowSize: WindowSizeSettings{FixedWindowSize: 4}}
		},
		"post quantum": func(s *MultiClientSettings) {
			s.DefaultPerformanceProfile = &PerformanceProfile{PostQuantumEncryption: true}
		},
	} {
		settings := DefaultMultiClientSettings()
		configure(settings)
		client := NewRemoteUserNatMultiClient(context.Background(), &testingEmptyMultiClientGenerator{}, nil, protocol.ProvideMode_Network, settings)
		want := client.config.Load().performanceProfile
		for kind, window := range client.windows {
			window.stateLock.Lock()
			got := window.performanceProfile
			window.stateLock.Unlock()
			if got != want {
				t.Errorf("%s: window %v holds %+v, the parent uses %+v", name, kind, got, want)
			}
		}
		client.Close()
	}
}
