package connect

import (
	"strings"
	"testing"
	"time"
)

type lockCheckingLogger struct {
	captureLogger
	buffer   *ReceiveBuffer
	unlocked bool
}

type lockCheckingVerbose struct{ logger *lockCheckingLogger }

func (l *lockCheckingLogger) V(int32) Verbose { return &lockCheckingVerbose{logger: l} }
func (v *lockCheckingVerbose) Enabled() bool {
	v.logger.unlocked = v.logger.buffer.mutex.TryLock()
	if v.logger.unlocked {
		v.logger.buffer.mutex.Unlock()
	}
	return true
}
func (v *lockCheckingVerbose) Info(...any)          {}
func (v *lockCheckingVerbose) Infof(string, ...any) {}

func TestReceiveHeadMismatchLogIsBounded(t *testing.T) {
	log := &captureLogger{enabled: true}
	throttle := newLogThrottle(time.Minute)
	pack := &ReceivePack{}
	now := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		logReceiveHeadMismatch(log, throttle, now, pack)
	}
	if len(log.verbose) != 1 {
		t.Fatalf("got %d log lines, want 1", len(log.verbose))
	}
	logReceiveHeadMismatch(log, throttle, now.Add(time.Minute), pack)
	if len(log.verbose) != 2 || !strings.Contains(log.verbose[1], "99 suppressed") {
		t.Fatalf("unexpected log lines: %v", log.verbose)
	}
}

func TestReceiveBufferPackHealsMissingOlderHead(t *testing.T) {
	buffer, pack, id, headKey, _ := newPackPanicFixture(t)
	pack.SequenceId = Id{1}
	id.SequenceId = pack.SequenceId
	oldID := id
	oldID.SequenceId = Id{}
	buffer.mutex.Lock()
	buffer.headReceiveSequenceIds[headKey] = oldID
	buffer.mutex.Unlock()
	retained := trackPackOwnership(pack)
	success, err := buffer.Pack(pack, time.Second)
	if !success || err != nil {
		t.Fatalf("Pack: success=%v err=%v", success, err)
	}
	finishInstalledSequence(t, buffer, id, headKey)
	assertPackOwnershipReleased(t, retained)
}
