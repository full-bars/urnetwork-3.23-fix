// This test-only process calls the unmodified platform probes from connect
// b1bee8132573c44bf93511533135212a37962bc0: net_extender_probe.go and
// net_extender_camouflage_probe.go. Its separate module prevents the fork's
// client implementation from being substituted for the platform's client.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"time"

	"github.com/urnetwork/connect"
)

type request struct {
	Mode            string
	Port            int
	PublicKey       []byte
	CamouflageKey   []byte
	DestinationHost string
	DestinationPort int
	APIURL          string
	Certificate     []byte
}

type result struct {
	Address   string
	PublicKey []byte
	Signature []byte
	Error     string
}

func main() {
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		panic(err)
	}
	out := run(req)
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		panic(err)
	}
}

func run(req request) result {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := connect.DefaultConnectSettings()
	settings.RequestTimeout = 8 * time.Second
	ip := netip.MustParseAddr("127.0.0.1")
	if req.Mode == "forward" {
		roots := x509.NewCertPool()
		cert, err := x509.ParseCertificate(req.Certificate)
		if err != nil {
			return result{Error: err.Error()}
		}
		roots.AddCert(cert)
		address, err := connect.ProbeExtenderForwardWithCamouflageKey(ctx, settings, ip, req.Port, "example.com", req.PublicKey, req.CamouflageKey, req.APIURL, &tls.Config{RootCAs: roots})
		if err != nil {
			return result{Error: err.Error()}
		}
		return result{Address: address}
	}
	var mode connect.ExtenderConnectMode
	switch req.Mode {
	case "tcp":
		mode = connect.ExtenderConnectModeTcpTls
	case "quic":
		mode = connect.ExtenderConnectModeQuic
	case "dns":
		mode = connect.ExtenderConnectModeDns
	default:
		return result{Error: fmt.Sprintf("unknown mode %q", req.Mode)}
	}
	response, err := connect.ProbeExtenderCarrierWithCamouflageKey(ctx, settings, ip, mode, req.Port, connect.DefaultExtenderDnsTld, "example.com", req.PublicKey, req.CamouflageKey, req.DestinationHost, req.DestinationPort)
	if err != nil {
		return result{Error: err.Error()}
	}
	return result{PublicKey: response.PublicKey, Signature: response.ChallengeSignature}
}
