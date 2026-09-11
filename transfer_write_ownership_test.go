package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ownershipWriter mimics MultiRouteSelector's documented contract: every
// failure path returns the frame to the message pool before reporting, so the
// caller must treat the frame as consumed.
type ownershipWriter struct {
	// outcome selects which documented failure the writer reproduces.
	failWithError bool
	timeout       bool
	// reliableOnlySupported reports whether the writer satisfies
	// transferReliableOnlyMultiRouteWriter. A writer that does not is the one
	// case where the frame never reaches a writer at all.
	returns int
}

func (self *ownershipWriter) consume(transferFrameBytes []byte) {
	self.returns += 1
	MessagePoolReturn(transferFrameBytes)
}

func (self *ownershipWriter) writeDetailedReliableOnly(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) (bool, transferWriteDisposition, error) {
	if self.failWithError {
		self.consume(transferFrameBytes)
		return false, transferWriteDisposition{}, errors.New("route write failed")
	}
	if self.timeout {
		self.consume(transferFrameBytes)
		return false, transferWriteDisposition{}, nil
	}
	return true, transferWriteDisposition{transportType: TransportTypeUnknown}, nil
}

func (self *ownershipWriter) Write(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) error {
	self.consume(transferFrameBytes)
	return errors.New("route write failed")
}

func (self *ownershipWriter) WriteDetailed(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) (bool, error) {
	return false, self.Write(ctx, transferFrameBytes, timeout)
}

func (self *ownershipWriter) GetActiveRoutes() []Route   { return nil }
func (self *ownershipWriter) GetInactiveRoutes() []Route { return nil }

// plainWriter satisfies MultiRouteWriter but not the reliable-only interface,
// which is the single path where a write helper fails before any writer has
// taken the frame.
type plainWriter struct {
	written int
}

func (self *plainWriter) Write(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) error {
	self.written += 1
	MessagePoolReturn(transferFrameBytes)
	return nil
}

func (self *plainWriter) WriteDetailed(
	ctx context.Context,
	transferFrameBytes []byte,
	timeout time.Duration,
) (bool, error) {
	return true, self.Write(ctx, transferFrameBytes, timeout)
}

func (self *plainWriter) GetActiveRoutes() []Route   { return nil }
func (self *plainWriter) GetInactiveRoutes() []Route { return nil }

// ownedFrame returns a pooled frame held by an owner plus the shared reference
// handed to a write helper, mirroring the real call sites: the sequence keeps
// its reference and shares a second one with the writer.
func ownedFrame(t *testing.T) (owned []byte, shared []byte) {
	t.Helper()
	owned = MessagePoolGet(512)
	pooled, _ := MessagePoolCheck(owned)
	if !pooled {
		t.Fatal("expected a pooled frame")
	}
	shared = MessagePoolShareReadOnly(owned)
	_, isShared := MessagePoolCheck(owned)
	if !isShared {
		t.Fatal("expected the frame to be shared after handing a reference to the writer")
	}
	return owned, shared
}

// assertOwnerReferenceIntact checks the owner's reference survived the write.
// A double return drops the count to zero and the frame goes back to the pool
// while the owner still points at it.
func assertOwnerReferenceIntact(t *testing.T, owned []byte, what string) {
	t.Helper()
	// MessagePoolCheck's shared flag is sticky once a frame has ever been
	// shared, so it cannot report the live count. pooled means the count is
	// still above zero, and a release that reports true means the owner held
	// the last reference — exactly one reference outstanding, as intended.
	pooled, _ := MessagePoolCheck(owned)
	if !pooled {
		t.Fatalf("%s: frame was freed while the owner still held a reference (double return)", what)
	}
	if !MessagePoolReturn(owned) {
		t.Fatalf("%s: owner did not hold the last reference (double return left the count wrong)", what)
	}
}

// The write helpers must not return a frame the writer already returned.
func TestWriteAckMultiRouteDoesNotDoubleReturnOnError(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &ownershipWriter{failWithError: true}

	if err := writeAckMultiRoute(writer, context.Background(), shared, time.Second); err == nil {
		t.Fatal("expected the write error to propagate")
	}
	if writer.returns != 1 {
		t.Fatalf("expected the writer to consume the frame once, got %d", writer.returns)
	}
	assertOwnerReferenceIntact(t, owned, "writeAckMultiRoute error path")
}

func TestWriteAckMultiRouteDoesNotDoubleReturnOnTimeout(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &ownershipWriter{timeout: true}

	if err := writeAckMultiRoute(writer, context.Background(), shared, time.Second); err == nil {
		t.Fatal("expected a timeout error")
	}
	assertOwnerReferenceIntact(t, owned, "writeAckMultiRoute timeout path")
}

func TestWriteMultiRouteWithCarrierDoesNotDoubleReturnOnError(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &ownershipWriter{failWithError: true}

	_, err := writeMultiRouteWithCarrier(writer, context.Background(), shared, time.Second, true)
	if err == nil {
		t.Fatal("expected the write error to propagate")
	}
	assertOwnerReferenceIntact(t, owned, "writeMultiRouteWithCarrier error path")
}

func TestWriteMultiRouteWithCarrierDoesNotDoubleReturnOnTimeout(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &ownershipWriter{timeout: true}

	_, err := writeMultiRouteWithCarrier(writer, context.Background(), shared, time.Second, true)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	assertOwnerReferenceIntact(t, owned, "writeMultiRouteWithCarrier timeout path")
}

// The one failure that happens before any writer takes the frame. Here the
// helper owns the frame and must return it, or it leaks.
func TestWriteMultiRouteWithCarrierReturnsFrameWhenReliableOnlyUnsupported(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &plainWriter{}

	_, err := writeMultiRouteWithCarrier(writer, context.Background(), shared, time.Second, true)
	if err == nil {
		t.Fatal("expected an error when the writer cannot honour reliableOnly")
	}
	if writer.written != 0 {
		t.Fatal("expected the frame never to reach the writer")
	}
	assertOwnerReferenceIntact(t, owned, "reliableOnly unsupported path")
}

// The success path leaves the frame with the writer, which releases it.
func TestWriteMultiRouteWithCarrierSuccessLeavesOwnerReference(t *testing.T) {
	owned, shared := ownedFrame(t)
	writer := &ownershipWriter{}

	disposition, err := writeMultiRouteWithCarrier(writer, context.Background(), shared, time.Second, true)
	if err != nil {
		t.Fatalf("expected the write to succeed, got %v", err)
	}
	if disposition.transportType == "" {
		t.Fatal("expected a transport type on the disposition")
	}
	// The writer took the shared reference and has not released it yet, so
	// the owner is not the last holder until the writer releases.
	if MessagePoolReturn(shared) {
		t.Fatal("expected the writer's reference to be one of two outstanding after a successful write")
	}
	assertOwnerReferenceIntact(t, owned, "success path")
}
