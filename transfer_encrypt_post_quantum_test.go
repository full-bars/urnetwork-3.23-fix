package connect

import (
	"context"
	"crypto/tls"
	"os"
	"testing"
	"time"
)

// resetTlsDefaultReceiverHybridCache puts the process-wide measurement back to
// the unmeasured state, so a test that replaces the probe starts from a clean
// cache and leaves one behind.
func resetTlsDefaultReceiverHybridCache(t *testing.T) {
	t.Helper()
	restore := func() {
		tlsDefaultReceiverHybridLock.Lock()
		defer tlsDefaultReceiverHybridLock.Unlock()
		tlsDefaultHybridProbe = measureTlsDefaultReceiverHybrid
		tlsDefaultReceiverHybridGodebug = ""
		tlsDefaultReceiverHybridOffers = false
		tlsDefaultReceiverHybridMeasured = false
		tlsDefaultReceiverHybridRetryAt = time.Time{}
	}
	restore()
	t.Cleanup(restore)
}

// tlsDefaultHybridCacheState reads the cached measurement and its retry time.
func tlsDefaultHybridCacheState() (offers bool, measured bool, godebug string, retryAt time.Time) {
	tlsDefaultReceiverHybridLock.Lock()
	defer tlsDefaultReceiverHybridLock.Unlock()
	return tlsDefaultReceiverHybridOffers,
		tlsDefaultReceiverHybridMeasured,
		tlsDefaultReceiverHybridGodebug,
		tlsDefaultReceiverHybridRetryAt
}

// The declaration is a claim about what this manager will actually serve: the
// session layer has to be on, the identity a requiring peer verifies against
// has to be set up, and the receiver-role TLS config has to offer the
// X25519MLKEM768 hybrid. Each guard below keeps a manager that cannot seal a
// post-quantum session from declaring that it can.

func TestServesPostQuantumEncryptionBranches(t *testing.T) {
	// the empty-list cases follow the process default, so pin the default
	t.Setenv("GODEBUG", "")
	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	selfCertPem := certificateToPemChain([]tls.Certificate{cert})

	hybridWithFallback := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
	}
	hybridOnly := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
	}
	classicalOnly := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519},
	}
	// The empty list is the crypto/tls default, which leads with the hybrid.
	defaultCurves := &tls.Config{Certificates: []tls.Certificate{cert}}

	clientKeyManager := &ClientKeyManager{}

	cases := []struct {
		name    string
		manager *EncryptionSessionManager
		serves  bool
	}{
		{
			name:    "no settings",
			manager: &EncryptionSessionManager{},
			serves:  false,
		},
		{
			name: "encryption off",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOff},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  hybridWithFallback,
				selfCertPem:      selfCertPem,
			},
			serves: false,
		},
		{
			name: "no client identity key manager",
			manager: &EncryptionSessionManager{
				settings:        &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				serverTlsConfig: hybridWithFallback,
				selfCertPem:     selfCertPem,
			},
			serves: false,
		},
		{
			name: "no receiver tls config",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				selfCertPem:      selfCertPem,
			},
			serves: false,
		},
		{
			name: "no identity certificate",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  hybridWithFallback,
			},
			serves: false,
		},
		{
			name: "classical curves only",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  classicalOnly,
				selfCertPem:      selfCertPem,
			},
			serves: false,
		},
		{
			name: "hybrid offered with fallback",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  hybridWithFallback,
				selfCertPem:      selfCertPem,
			},
			serves: true,
		},
		{
			name: "hybrid only",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  hybridOnly,
				selfCertPem:      selfCertPem,
			},
			serves: true,
		},
		{
			name: "default curves",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  defaultCurves,
				selfCertPem:      selfCertPem,
			},
			serves: true,
		},
		{
			name: "required mode",
			manager: &EncryptionSessionManager{
				settings:         &EncryptionSettings{Mode: EncryptionModeRequired},
				clientKeyManager: clientKeyManager,
				serverTlsConfig:  hybridWithFallback,
				selfCertPem:      selfCertPem,
			},
			serves: true,
		},
	}
	for _, c := range cases {
		if got := c.manager.ServesPostQuantumEncryption(); got != c.serves {
			t.Fatalf("%s: ServesPostQuantumEncryption() = %t, want %t", c.name, got, c.serves)
		}
	}
}

// X25519MLKEM768 is a TLS 1.3 key exchange, so a receiver config whose version
// interval excludes TLS 1.3 cannot negotiate it whatever its curve list names.
// A zero bound is the crypto/tls default for that end of the interval and
// leaves TLS 1.3 available.
func TestServesPostQuantumEncryptionRequiresTls13(t *testing.T) {
	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	selfCertPem := certificateToPemChain([]tls.Certificate{cert})
	clientKeyManager := &ClientKeyManager{}

	newManager := func(config *tls.Config) *EncryptionSessionManager {
		return &EncryptionSessionManager{
			settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
			clientKeyManager: clientKeyManager,
			serverTlsConfig:  config,
			selfCertPem:      selfCertPem,
		}
	}
	hybrid := func(minVersion, maxVersion uint16) *tls.Config {
		return &tls.Config{
			Certificates:     []tls.Certificate{cert},
			MinVersion:       minVersion,
			MaxVersion:       maxVersion,
			CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
		}
	}

	cases := []struct {
		name   string
		config *tls.Config
		serves bool
	}{
		{
			name:   "tls 1.3 only",
			config: hybrid(tls.VersionTLS13, tls.VersionTLS13),
			serves: true,
		},
		{
			name:   "default interval",
			config: hybrid(0, 0),
			serves: true,
		},
		{
			name:   "tls 1.2 maximum with the hybrid group",
			config: hybrid(0, tls.VersionTLS12),
			serves: false,
		},
		{
			name:   "tls 1.2 only",
			config: hybrid(tls.VersionTLS12, tls.VersionTLS12),
			serves: false,
		},
		{
			name:   "minimum above tls 1.3",
			config: hybrid(tls.VersionTLS13+1, 0),
			serves: false,
		},
		{
			name: "tls 1.2 maximum with the default groups",
			config: &tls.Config{
				Certificates: []tls.Certificate{cert},
				MaxVersion:   tls.VersionTLS12,
			},
			serves: false,
		},
	}
	for _, c := range cases {
		if got := newManager(c.config).ServesPostQuantumEncryption(); got != c.serves {
			t.Fatalf("%s: ServesPostQuantumEncryption() = %t, want %t", c.name, got, c.serves)
		}
	}
}

// An empty CurvePreferences is the crypto/tls default group list, and
// crypto/tls leaves X25519MLKEM768 out of that list when the process restores
// the pre-go 1.24 key exchanges with GODEBUG=tlsmlkem=0. The claim follows the
// list the receiver will actually negotiate; an explicit list is taken as given
// with or without the setting.
func TestServesPostQuantumEncryptionDefaultFollowsTlsMlKem(t *testing.T) {
	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	selfCertPem := certificateToPemChain([]tls.Certificate{cert})
	clientKeyManager := &ClientKeyManager{}

	newManager := func(config *tls.Config) *EncryptionSessionManager {
		return &EncryptionSessionManager{
			settings:         &EncryptionSettings{Mode: EncryptionModeOpportunistic},
			clientKeyManager: clientKeyManager,
			serverTlsConfig:  config,
			selfCertPem:      selfCertPem,
		}
	}
	defaultGroups := &tls.Config{Certificates: []tls.Certificate{cert}}
	hybridGroups := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
	}

	t.Setenv("GODEBUG", "tlsmlkem=1")
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), true)
	AssertEqual(t, newManager(defaultGroups).ServesPostQuantumEncryption(), true)
	AssertEqual(t, newManager(hybridGroups).ServesPostQuantumEncryption(), true)

	t.Setenv("GODEBUG", "tlsmlkem=0")
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, newManager(defaultGroups).ServesPostQuantumEncryption(), false)
	// the setting changes the default list only, never an explicit one
	AssertEqual(t, newManager(hybridGroups).ServesPostQuantumEncryption(), true)

	t.Setenv("GODEBUG", "")
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), true)
	AssertEqual(t, newManager(defaultGroups).ServesPostQuantumEncryption(), true)
}

// The receiver-role TLS config has to offer the X25519MLKEM768 hybrid for a
// session to be post-quantum. A caller-supplied config that leaves it out
// serves classical sessions only, and does not advertise.
func TestServesPostQuantumEncryptionNeedsTheHybridGroup(t *testing.T) {
	// the empty-list case follows the process default, so pin the default
	t.Setenv("GODEBUG", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	classical := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientAuth:       tls.RequireAnyClientCert,
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
	}
	client := testingProvideClient(t, ctx, EncryptionModeOpportunistic, classical)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), false)
	frame, err := client.ContractManager().provideFrame()
	AssertEqual(t, err, nil)
	advertised, present := testingProvidePostQuantumEncryption(t, frame)
	AssertEqual(t, present, false)
	AssertEqual(t, advertised, false)

	// the crypto/tls default list leads with the hybrid
	hybridDefault := classical.Clone()
	hybridDefault.CurvePreferences = nil
	client = testingProvideClient(t, ctx, EncryptionModeOpportunistic, hybridDefault)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), true)
	frame, err = client.ContractManager().provideFrame()
	AssertEqual(t, err, nil)
	advertised, present = testingProvidePostQuantumEncryption(t, frame)
	AssertEqual(t, present, true)
	AssertEqual(t, advertised, true)
}

// A probe that does not complete says nothing about the default list. The claim
// answers false for the retry interval, which bounds how often an incomplete
// probe runs, and the list is measured again afterwards, so a deadline that
// fired during a pause in the process does not decide the answer for the life
// of the process.
func TestServesPostQuantumEncryptionRecoversFromIncompleteProbe(t *testing.T) {
	resetTlsDefaultReceiverHybridCache(t)
	t.Setenv("GODEBUG", "")

	probeCalls := 0
	tlsDefaultHybridProbe = func() (bool, bool) {
		probeCalls++
		// a deadline expired before the receiver answered
		return false, false
	}

	// the first claim takes an incomplete measurement and answers conservatively
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, probeCalls, 1)
	offers, measured, godebug, retryAt := tlsDefaultHybridCacheState()
	AssertEqual(t, offers, false)
	AssertEqual(t, measured, false)
	AssertEqual(t, godebug, "")
	if !retryAt.After(time.Now()) {
		t.Fatalf("an incomplete probe must schedule a retry, retryAt = %v", retryAt)
	}

	// inside the retry interval the conservative answer holds without probing
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, probeCalls, 1)

	// the pause clears; once the interval has passed, the next claim measures
	// again and follows what it finds
	tlsDefaultHybridProbe = func() (bool, bool) {
		probeCalls++
		return true, true
	}
	tlsDefaultReceiverHybridLock.Lock()
	tlsDefaultReceiverHybridRetryAt = time.Time{}
	tlsDefaultReceiverHybridLock.Unlock()
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), true)
	AssertEqual(t, probeCalls, 2)

	// the completed answer is kept
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), true)
	AssertEqual(t, probeCalls, 2)
}

// The cache key has to be the GODEBUG value the measurement ran under. A
// measurement that cannot be attributed to one setting is not kept, so a claim
// is never answered from a measurement taken under a different setting. The
// window between a read taken before the lock and the lock itself cannot be
// forced deterministically from a test; a change while the probe runs covers the
// same requirement, because the result then belongs to neither value.
func TestServesPostQuantumEncryptionKeysTheCacheOnTheMeasuredGodebug(t *testing.T) {
	resetTlsDefaultReceiverHybridCache(t)
	t.Setenv("GODEBUG", "")

	probeCalls := 0
	tlsDefaultHybridProbe = func() (bool, bool) {
		probeCalls++
		// the setting changes while the handshake runs
		os.Setenv("GODEBUG", "tlsmlkem=0")
		return true, true
	}
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, probeCalls, 1)
	// the answer is not kept: it belongs to neither setting
	offers, measured, _, _ := tlsDefaultHybridCacheState()
	AssertEqual(t, offers, false)
	AssertEqual(t, measured, false)

	// the setting is stable now; that measurement is kept under the value it ran
	// under, and the next claim is answered from the cache
	tlsDefaultHybridProbe = func() (bool, bool) {
		probeCalls++
		return false, true
	}
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, probeCalls, 2)
	offers, measured, godebug, _ := tlsDefaultHybridCacheState()
	AssertEqual(t, offers, false)
	AssertEqual(t, measured, true)
	AssertEqual(t, godebug, "tlsmlkem=0")
	AssertEqual(t, tlsDefaultReceiverOffersHybrid(), false)
	AssertEqual(t, probeCalls, 2)
}
