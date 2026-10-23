package main

import (
	"os"
	"strings"
)

// tlsHelloChromeEnv reports whether the operator asked for the Chrome tls
// client hello at startup with URNETWORK_TLS_HELLO. It is OFF unless the
// variable is on, 1, true or yes. This is only the startup default for the
// runtime gate: the tls_hello control key overrides it and can be changed
// without a restart (see resolveTlsHello).
func tlsHelloChromeEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_TLS_HELLO"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// resolveTlsHello is whether new connections should present the Chrome hello:
// the tls_hello control key when it has been set, otherwise
// URNETWORK_TLS_HELLO. A persisted off beats an environment that says on, and
// clearing the key hands the decision back to the environment, which is what
// a restart would use.
func resolveTlsHello(state *controlState) bool {
	if state != nil {
		if v, ok := state.get("tls_hello"); ok {
			return strings.EqualFold(v, "on")
		}
	}
	return tlsHelloChromeEnv()
}

// resolveTlsHelloName is resolveTlsHello as the control socket's on|off value,
// so an apply always logs and reports the hello that is in force rather than
// the raw spelling it arrived with.
func resolveTlsHelloName(state *controlState) string {
	return onOff(resolveTlsHello(state))
}
