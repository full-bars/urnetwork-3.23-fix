package connect

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"

	_ "embed"
)

// the let's encrypt root CAs as defined at https://letsencrypt.org/certificates/
// this includes:
// - ISRG Root X1
// - ISRG Root X2
//
//go:embed net_tls_ca.pem
var tlsCaPem string

func PinnedCertPool() (*x509.CertPool, error) {

	certPool := x509.NewCertPool()

	if !certPool.AppendCertsFromPEM([]byte(tlsCaPem)) {
		return nil, fmt.Errorf("Could not append ca certs")
	}

	return certPool, nil
}

// clientTlsSessionCacheCapacity bounds one dial path's session ticket cache.
const clientTlsSessionCacheCapacity = 16

// The application protocols of the two client dial paths: an api request can
// negotiate h2, while a websocket upgrade requires http/1.1, so the paths
// keep separate tls dialers and ALPN offers (net_tls_hello.go applies the
// Chrome hello only where the offered protocols can be presented).
var (
	clientHttpNextProtos      = []string{"h2", "http/1.1"}
	clientWebSocketNextProtos = []string{"http/1.1"}
)

// newClientTlsConfig gives one dial path its own TLS session ticket cache, so a
// re-dial of a server resumes and skips the certificate chain verification that
// otherwise made up ~13% of a busy node's allocations (alloc profile,
// 2026-10-15). The cache belongs to the path, not the process: a ticket from one
// path is never offered on another, so a server cannot link the independent
// egress paths (direct, each resilient mode, each proxy identity) through a
// redeemed ticket. Same helper and rule as upstream. When nextProtos is non-nil
// it replaces the config's application protocols.
func newClientTlsConfig(base *tls.Config, nextProtos []string) *tls.Config {
	var config *tls.Config
	if base == nil {
		config = &tls.Config{}
	} else {
		config = base.Clone()
	}
	if nextProtos != nil {
		config.NextProtos = append([]string(nil), nextProtos...)
	}
	// Always replace a caller-supplied cache: sharing the base config's would
	// make every derived path share tickets.
	config.ClientSessionCache = tls.NewLRUClientSessionCache(clientTlsSessionCacheCapacity)
	return config
}

var (
	sharedPinnedCertPoolOnce sync.Once
	sharedPinnedCertPool     *x509.CertPool
	sharedPinnedCertPoolErr  error
)

// sharedDefaultPinnedCertPool parses the embedded pinned roots once for every
// default client configuration. A provider builds a config per proxy identity
// (hundreds to thousands, several each), and each used to parse the same PEM and
// keep its own copy of the pool. Verification only reads RootCAs, so one pool
// is shared; PinnedCertPool stays the fresh-pool API for a caller that plans to
// modify its result. Same change as upstream's sharedDefaultPinnedCertPool.
func sharedDefaultPinnedCertPool() (*x509.CertPool, error) {
	sharedPinnedCertPoolOnce.Do(func() {
		sharedPinnedCertPool, sharedPinnedCertPoolErr = PinnedCertPool()
	})
	return sharedPinnedCertPool, sharedPinnedCertPoolErr
}

func DefaultTlsConfig() (*tls.Config, error) {
	certPool, err := sharedDefaultPinnedCertPool()
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		RootCAs:    certPool,
		MinVersion: tls.VersionTLS12,
	}
	return tlsConfig, nil
}

// RFC 5246
type TlsContentType = byte

const (
	TlsContentTypeChangeCipherSpec TlsContentType = 0x14
	TlsContentTypeAlert            TlsContentType = 0x15
	TlsContentTypeHandshake        TlsContentType = 0x16
	TlsContentTypeApplicationData  TlsContentType = 0x17
	TlsContentTypeHeartbeat        TlsContentType = 0x18
)
