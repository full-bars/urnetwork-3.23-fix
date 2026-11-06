package connect

// Test helpers net_extender_activate_*_test.go share, taken unchanged from the
// modern connect tree's own test files.

import (
	"crypto/ecdh"
	"crypto/rand"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// A host's clock as time.Now reads it in a running process: each reading
// carries a monotonic reading beside its wall one, and Go compares and
// subtracts two such readings by their monotonic parts alone. `awake` moves
// both; `sleep` moves the wall reading alone, which is what a host that
// sleeps does to a Go process (mach_absolute_time on darwin and
// CLOCK_MONOTONIC on linux and android stop while it sleeps); `setBack` moves
// the wall reading alone the other way, as a clock correction does.
//
// The readings are made from one real reading, the only source of a monotonic
// part. time.Time offers no way to set one, so a reading's unexported
// monotonic field is set through reflect, and the constructor checks that
// what it makes compares the way it must before any test relies on it.
type testHostClock struct {
	stateLock sync.Mutex
	// a real reading
	base time.Time
	// how far each part of a reading has moved from base
	monotonicOffset time.Duration
	wallOffset      time.Duration
}

// A host clock from the real reading taken now, checked to compare by its
// monotonic part the way time.Now's readings do.
func newTestHostClock(t *testing.T) *testHostClock {
	t.Helper()
	base := time.Now()
	if base == base.Round(0) {
		t.Fatal("time.Now carries no monotonic reading here")
	}
	self := &testHostClock{
		base: base,
	}
	// a reading after an hour of sleep must compare by its monotonic part,
	// which has not moved, and keep its wall part, which has
	slept, ok := self.reading(0, time.Hour)
	if !ok || slept.Sub(base) != 0 || slept.Round(0).Sub(base.Round(0)) != time.Hour {
		t.Fatal("this Go version's time.Time cannot carry a monotonic reading apart from its wall one")
	}
	return self
}

// The reading whose monotonic and wall parts are the given offsets past base,
// and whether it could be made.
func (self *testHostClock) reading(monotonicOffset time.Duration, wallOffset time.Duration) (time.Time, bool) {
	reading := self.base.Add(wallOffset)
	if reading == reading.Round(0) {
		return time.Time{}, false
	}
	// the monotonic part (time.Time's ext while a reading carries one), which
	// Add moved with the wall part
	monotonic := reflect.ValueOf(&reading).Elem().FieldByName("ext")
	if !monotonic.IsValid() || monotonic.Kind() != reflect.Int64 {
		return time.Time{}, false
	}
	*(*int64)(unsafe.Pointer(monotonic.UnsafeAddr())) += int64(monotonicOffset - wallOffset)
	return reading, true
}

// The reading the host's time.Now would return at this point.
func (self *testHostClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reading, _ := self.reading(self.monotonicOffset, self.wallOffset)
	return reading
}

// The host runs for `d`: both parts move.
func (self *testHostClock) awake(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.monotonicOffset += d
	self.wallOffset += d
}

// The host sleeps for `d`: the wall part moves and the monotonic part stays.
func (self *testHostClock) sleep(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.wallOffset += d
}

// The wall clock is set back by `d`, and the monotonic part stays.
func (self *testHostClock) setBack(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.wallOffset -= d
}

// The next check the loop arms, which it does once it has finished with
// `what`.
func nextTestResumeCheck(t *testing.T, armedChecks <-chan chan time.Time, what string) chan time.Time {
	t.Helper()
	select {
	case check := <-armedChecks:
		return check
	case <-time.After(10 * time.Second):
		t.Fatalf("the loop armed no check after %s", what)
		return nil
	}
}

func newTestCamouflageKey(t *testing.T) []byte {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey.PublicKey().Bytes()
}
