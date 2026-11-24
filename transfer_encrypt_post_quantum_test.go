package connect

import (
	"context"
	"crypto/tls"
	"testing"
)

// The declaration is a claim about what this manager will actually serve: the
// session layer has to be on, the identity a requiring peer verifies against
// has to be set up, and the receiver-role TLS config has to offer the
// X25519MLKEM768 hybrid. Each guard below keeps a manager that cannot seal a
// post-quantum session from declaring that it can.

func TestServesPostQuantumEncryptionBranches(t *testing.T) {
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

// The receiver-role TLS config has to offer the X25519MLKEM768 hybrid for a
// session to be post-quantum. A caller-supplied config that leaves it out
// serves classical sessions only, and does not advertise.
func TestServesPostQuantumEncryptionNeedsTheHybridGroup(t *testing.T) {
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
