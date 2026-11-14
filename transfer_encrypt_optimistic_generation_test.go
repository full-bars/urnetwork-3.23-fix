package connect

import (
	"bytes"
	"testing"
)

func TestOptimisticHandshakeGenerationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		active, wire Id
		payload      []byte
		want         bool
	}{
		{"matching", Id{15: 2}, Id{15: 2}, []byte{23, 1}, true},
		{"older", Id{15: 2}, Id{15: 1}, []byte{23, 1}, false},
		{"newer", Id{15: 2}, Id{15: 3}, []byte{23, 1}, false},
		{"untagged into named", Id{15: 2}, Id{}, []byte{23, 1}, false},
		{"legacy", Id{}, Id{}, []byte{20, 1}, true},
		{"client hello", Id{15: 2}, Id{15: 2}, []byte{22, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
			defer cleanup()
			e := injectTestEpochWithId(sess, false, nil, tc.active)
			defer e.cancel()
			e.transport = newSequenceTlsTransport(e.ctx)
			e.serverFlightSent = true
			sess.optimisticallyDeliverHandshakeForEpoch(tc.payload, tc.wire)
			e.transport.inboxLock.Lock()
			got := bytes.Clone(e.transport.inboxBuf)
			e.transport.inboxLock.Unlock()
			var want []byte
			if tc.want {
				want = tc.payload
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got %x want %x", got, want)
			}
			if sess.currentEpoch() != e {
				t.Fatal("optimistic delivery changed the epoch")
			}
		})
	}
}
