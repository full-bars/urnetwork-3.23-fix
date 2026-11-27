package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestPostQuantumAdvertisementWithGetCertificate(t *testing.T) {
	t.Setenv("GODEBUG", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	published, err := generateSequenceTlsCertificate()
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := generateSequenceTlsCertificate()
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{
		Certificates:     []tls.Certificate{published},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return &alternate, nil
		},
	}
	resolved, committed, _, err := resolveReceiveTlsConfig(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GetCertificate == nil {
		t.Fatal("resolved config lost GetCertificate")
	}
	selected, err := resolved.GetCertificate(&tls.ClientHelloInfo{ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(selected.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := verifyPeerCertificateAgainstContract([]*x509.Certificate{leaf}, committed); ok || err == nil {
		t.Fatalf("alternate callback leaf matched the published chain: ok=%t err=%v", ok, err)
	}

	for _, tc := range []struct {
		name   string
		config *tls.Config
		want   bool
	}{
		{name: "callback can replace leaf", config: config, want: false},
		{name: "static identity", config: &tls.Config{Certificates: []tls.Certificate{published}, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testingProvideClient(t, ctx, EncryptionModeOpportunistic, tc.config)
			if got := client.EncryptionSessionManager().ServesPostQuantumEncryption(); got != tc.want {
				t.Fatalf("ServesPostQuantumEncryption() = %t, want %t", got, tc.want)
			}
			frame, err := client.ContractManager().provideFrame()
			if err != nil {
				t.Fatal(err)
			}
			advertised, present := testingProvidePostQuantumEncryption(t, frame)
			if advertised != tc.want || present != tc.want {
				t.Fatalf("PQ advertised=%t present=%t, want both %t", advertised, present, tc.want)
			}
		})
	}
}
