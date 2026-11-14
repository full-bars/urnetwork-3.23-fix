package connect

import (
	"errors"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// An unauthenticated maximum epoch must not block the legitimate handshake.
func TestServerEpochIdPoisoning(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleServer)
	defer cleanup()
	var maxEpochId Id
	for i := range maxEpochId {
		maxEpochId[i] = 0xff
	}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
		SessionRole: sequenceTlsRoleClient.toProtobuf(),
		EpochId:     maxEpochId.Bytes(), Payload: []byte{0},
	})
	sess.completeHandshake(sess.currentEpoch(), errors.New("timeout"))
	legitEpochId := Id{15: 1}
	sess.DeliverEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
		SessionRole: sequenceTlsRoleClient.toProtobuf(),
		EpochId:     legitEpochId.Bytes(), Payload: []byte{22, 3, 3, 0, 100, 1, 0, 0, 96},
	})
	if e := sess.currentEpoch(); e == nil || sess.epochIdOf(e) != legitEpochId || isClosed(e.handshakeDone) {
		t.Fatalf("server wedged on %s; dropped ClientHello %s", sess.epochIdOf(e), legitEpochId)
	}
}
