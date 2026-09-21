//go:build linux

package main

import (
	"testing"
)

// TestPeerAllowed_RootAccepted verifies that uid 0 (root) is always accepted
// by the provider's control socket, regardless of the provider's UID. This
// was the root cause of shakedown bugs 1 and 2: urnet-tools invoked as root
// was rejected by verifyPeerCredentials, causing update verification to
// fail ("restart did not take effect") and control socket set/clear to fail
// with "connection reset by peer".
func TestPeerAllowed_RootAccepted(t *testing.T) {
	tests := []struct {
		name        string
		peerUID     uint32
		providerUID uint32
		want        bool
	}{
		{"root manages non-root provider", 0, 1000, true},
		{"root manages root provider", 0, 0, true},
		{"same user", 1000, 1000, true},
		{"different non-root user rejected", 1001, 1000, false},
		{"foreign user rejected", 65534, 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerAllowed(tt.peerUID, tt.providerUID)
			if got != tt.want {
				t.Errorf("peerAllowed(%d, %d) = %v, want %v", tt.peerUID, tt.providerUID, got, tt.want)
			}
		})
	}
}
