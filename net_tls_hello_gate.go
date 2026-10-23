package connect

// net_tls_hello_gate.go -- the process-wide runtime switch for the tls client
// hello (net_tls_hello.go).
//
// The provider's control socket flips it (`tls_hello` on|off, with
// URNETWORK_TLS_HELLO as the startup default) so an operator can compare the
// Chrome hello against Go's on one running box. A restart would drop every
// proxy and its clients, so interleaved on and off windows measured the
// restarts instead of the hello.
//
// The gate is read on every dial and never cached at construction: a dial path
// built before the change presents the new hello on its next connection, while
// a connection that is already open keeps the handshake it made. Nothing is
// reconnected by the change.
//
// It is tri-state. Unset, the zero value, leaves the choice to the strategy's
// ConnectSettings.TlsClientHelloFingerprint -- what a library user who never
// touches the control socket gets. Set, it overrides that base for every dial
// in the process, in both directions: the provider sets it at startup from the
// persisted key or the environment, so its dials follow the gate and not a
// per-strategy default.

import "sync/atomic"

const (
	tlsHelloUnset int32 = iota
	tlsHelloChrome
	tlsHelloGo
)

var tlsHelloGate atomic.Int32

// SetTlsClientHelloChrome switches the Chrome hello on or off for every new
// connection in the process, overriding every strategy's
// ConnectSettings.TlsClientHelloFingerprint, and reports whether it was on
// before. It returns false when the gate had not been set, which is the same
// hello the unset gate presents (Go's), so a first `on` reads as `(was off)`.
//
// Open connections are untouched: each keeps the handshake it made. A change
// therefore takes effect one connection at a time, as the dial paths redial.
func SetTlsClientHelloChrome(chrome bool) (previous bool) {
	next := tlsHelloGo
	if chrome {
		next = tlsHelloChrome
	}
	return tlsHelloGate.Swap(next) == tlsHelloChrome
}

// ResetTlsClientHelloGate returns the process to the unset state, where each
// strategy's ConnectSettings.TlsClientHelloFingerprint decides again. Without
// it the first set would override every strategy for the life of the process,
// which library embedders and tests cannot undo.
func ResetTlsClientHelloGate() {
	tlsHelloGate.Store(tlsHelloUnset)
}

// TlsClientHelloChrome reports whether the gate has been set to the Chrome
// hello. An unset gate reads false: its effective default is Go's hello.
func TlsClientHelloChrome() bool {
	return tlsHelloGate.Load() == tlsHelloChrome
}

// tlsClientHelloFingerprint is the fingerprint a dial presents: the gate when
// it has been set, otherwise the strategy's base.
func tlsClientHelloFingerprint(base string) string {
	switch tlsHelloGate.Load() {
	case tlsHelloChrome:
		return TlsClientHelloFingerprintChrome
	case tlsHelloGo:
		return TlsClientHelloFingerprintGo
	}
	return base
}
