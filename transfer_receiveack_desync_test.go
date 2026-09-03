// Regression tests for the receiveAck desync-gating fix (PR-fix commit
// 8f1e2b3b, transfer.go receiveAck): when the cumulative-ack walk finds a
// sendItems entry missing from resendQueue, a RETAINED item is the expected
// (backstop-drop) case and stays quiet, but a NON-RETAINED item missing from
// resendQueue is a genuine resendQueue/sendItems desync that must be logged
// loudly (Errorf) rather than silently swallowed at the old V(2) level.
package connect

import (
	"fmt"
	"testing"
	"time"
)

// recordingLogger implements Logger, capturing Errorf calls so tests can
// assert on which log level a code path uses without depending on glog
// output capture.
type recordingLogger struct {
	errors []string
}

func (l *recordingLogger) Info(args ...any)                    {}
func (l *recordingLogger) Infof(format string, args ...any)    {}
func (l *recordingLogger) Warningf(format string, args ...any) {}
func (l *recordingLogger) Errorf(format string, args ...any) {
	l.errors = append(l.errors, fmt.Sprintf(format, args...))
}

// V reports every level disabled (via the package's noopVerbose), so
// V(1)/V(2)-guarded log calls in the code under test are skipped and kept
// out of the assertions here.
func (l *recordingLogger) V(level int32) Verbose { return noopVerbose{} }

// TestReceiveAckLogsErrorForNonRetainedDesync builds the exact state the
// desync guard exists to catch: a non-retained item present in sendItems but
// missing from resendQueue (i.e., NOT via the legitimate backstop-drop path,
// which only ever removes retained items this way). The cumulative ack walk
// must log this loudly via Errorf.
func TestReceiveAckLogsErrorForNonRetainedDesync(t *testing.T) {
	_, seq := newH1TestSequence(t)
	logger := &recordingLogger{}
	seq.log = logger

	desynced := &sendItem{
		transferItem: transferItem{
			messageId:      NewId(),
			sequenceNumber: 1,
		},
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: false, // non-retained: missing from resendQueue is unexplained
	}
	ackTarget := &sendItem{
		transferItem: transferItem{
			messageId:      NewId(),
			sequenceNumber: 2,
		},
		ackCallback: func(err error) {},
	}

	// desynced is in sendItems but deliberately NOT added to resendQueue.
	seq.sendItems = append(seq.sendItems, desynced, ackTarget)
	seq.resendQueue.Add(ackTarget)

	seq.receiveAck(ackTarget.messageId, false, nil)

	if len(logger.errors) != 1 {
		t.Fatalf("Errorf call count = %d, want 1 (non-retained desync must be logged loudly): %v", len(logger.errors), logger.errors)
	}
	t.Logf("logged: %s", logger.errors[0])
}

// TestReceiveAckQuietForRetainedDroppedDesync is the inverse: a RETAINED
// item missing from resendQueue is the expected shape of a backstop drop
// that raced the cumulative ack walk. It must NOT trigger the Errorf desync
// path.
func TestReceiveAckQuietForRetainedDroppedDesync(t *testing.T) {
	_, seq := newH1TestSequence(t)
	logger := &recordingLogger{}
	seq.log = logger

	droppedRetained := &sendItem{
		transferItem: transferItem{
			messageId:      NewId(),
			sequenceNumber: 1,
		},
		ackCallback:           func(err error) {},
		retainAfterAckTimeout: true, // retained: backstop drop is the expected explanation
		backstopDeadline:      time.Now().Add(-time.Second),
	}
	ackTarget := &sendItem{
		transferItem: transferItem{
			messageId:      NewId(),
			sequenceNumber: 2,
		},
		ackCallback: func(err error) {},
	}

	// droppedRetained is in sendItems but NOT in resendQueue — as it would be
	// immediately after dropItem removed it from the queue but before this
	// ack walk observed sendItems.
	seq.sendItems = append(seq.sendItems, droppedRetained, ackTarget)
	seq.resendQueue.Add(ackTarget)

	seq.receiveAck(ackTarget.messageId, false, nil)

	if len(logger.errors) != 0 {
		t.Fatalf("Errorf fired for a retained/dropped item (expected case, must stay quiet): %v", logger.errors)
	}
}
