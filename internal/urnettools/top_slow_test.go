package urnettools

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// A provider on a starved box answers slowly; it is not gone. Reading one late
// or missed answer as "lost the provider" flapped the screen to DISCONNECTED and
// filled the Events panel, and piling extra polls onto a struggling provider
// made it worse. These pin the behavior that replaces it: keep the last data
// with a SLOW marker, back off, shed optional polls, and only declare the
// provider lost after a long silence.

var errSlow = errors.New("read unix @->/home/u/.urnetwork/provider.sock: i/o timeout")

func slowModel(t *testing.T) (*topModel, *fakeClock) {
	t.Helper()
	m, clock := liveModel(t, time.Second)
	m.markAnswered(clock.Now()) // the fixture's first snapshot was an answer
	return m, clock
}

// fail runs one snapshot request that times out after took.
func failFetch(t *testing.T, m *topModel, clock *fakeClock, took time.Duration) {
	t.Helper()
	gen, _, ok := m.wantFetch(clock.Now())
	if !ok {
		t.Fatalf("no fetch wanted at %v (backoff %v)", clock.Now().Sub(topBase), m.backoff)
	}
	clock.Advance(took)
	m.apply(gen, nil, errSlow)
}

func TestTopOneMissedAnswerIsSlowNotLost(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	failFetch(t, m, clock, 5*time.Second)
	if m.conn != topConnected {
		t.Fatalf("one timeout flipped the screen to %v; a recent answer means slow, not lost", m.conn)
	}
	if !m.isSlow() {
		t.Fatal("a missed answer must mark the provider slow")
	}
	for _, e := range m.events {
		if strings.Contains(e.Text, "lost the provider") {
			t.Fatalf("a slow provider was reported lost: %q", e.Text)
		}
	}
	if m.snap == nil {
		t.Fatal("the last good data must stay on screen")
	}
}

func TestTopLostOnlyAfterALongSilence(t *testing.T) {
	m, clock := slowModel(t)
	// Keep failing: slow at first, lost once nothing has answered for the grace.
	var lostAt time.Duration
	for i := 0; i < 80 && m.conn == topConnected; i++ {
		clock.Advance(time.Second)
		if gen, _, ok := m.wantFetch(clock.Now()); ok {
			clock.Advance(3 * time.Second)
			m.apply(gen, nil, errSlow)
		}
		if m.conn == topDisconnected {
			lostAt = clock.Now().Sub(topBase)
		}
	}
	if m.conn != topDisconnected {
		t.Fatal("a provider silent for longer than the grace must be lost")
	}
	if lostAt < topLossGrace {
		t.Fatalf("declared lost after only %v; the grace is %v", lostAt, topLossGrace)
	}
	if !strings.Contains(m.lastErr, "i/o timeout") {
		t.Fatalf("the reason must survive into the disconnected view, got %q", m.lastErr)
	}
}

func TestTopNeverAnsweredIsStillLostAtOnce(t *testing.T) {
	// No data yet: there is nothing to keep showing, so say so straight away.
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	gen, _, _ := m.wantFetch(clock.Now())
	m.apply(gen, nil, errSlow)
	if m.conn != topDisconnected {
		t.Fatalf("no data and no answer: conn = %v, want disconnected", m.conn)
	}
}

func TestTopBackoffGrowsAndIsCapped(t *testing.T) {
	m, clock := slowModel(t)
	var seen []time.Duration
	for i := 0; i < 4; i++ {
		clock.Advance(20 * time.Second) // spaced out; each failure is within... reset the grace
		m.markAnswered(clock.Now())
		failFetch(t, m, clock, time.Second)
		seen = append(seen, m.backoff)
	}
	want := []time.Duration{topMinBackoff, 2 * topMinBackoff, 4 * topMinBackoff, topMaxBackoff}
	for i := range want {
		if i == len(want)-1 {
			if seen[i] > topMaxBackoff {
				t.Errorf("backoff %v exceeds the cap %v", seen[i], topMaxBackoff)
			}
			continue
		}
		if seen[i] != want[i] {
			t.Errorf("backoff after failure %d = %v, want %v (all: %v)", i+1, seen[i], want[i], seen)
		}
	}
}

func TestTopBackoffSpacesTheHeavyPollAndClearsOnAnswer(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	failFetch(t, m, clock, time.Second)
	back := m.backoff
	if back <= 0 {
		t.Fatal("no backoff after a failure")
	}
	// Inside interval+backoff: no new snapshot request.
	clock.Advance(topSnapshotEvery)
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("asked again before the backoff passed; that is the pile-on this exists to stop")
	}
	clock.Advance(back + time.Second)
	gen, _, ok := m.wantFetch(clock.Now())
	if !ok {
		t.Fatal("not asked again after the backoff")
	}
	clock.Advance(100 * time.Millisecond) // a prompt answer
	m.apply(gen, topFlowing(t), nil)
	if m.isSlow() || m.backoff != 0 {
		t.Fatalf("a prompt answer must clear slow and the backoff (slow=%v backoff=%v)", m.isSlow(), m.backoff)
	}
}

// A reply that arrives but takes seconds is the same signal: the provider is
// struggling, so the next polls are spaced by how long that one took.
func TestTopSlowButSuccessfulReplyBacksOff(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(3 * time.Second)
	m.apply(gen, topFlowing(t), nil)
	if m.conn != topConnected || m.backoff < 3*time.Second {
		t.Fatalf("a 3s reply: conn=%v backoff=%v, want connected and >= 3s of spacing", m.conn, m.backoff)
	}
}

func TestTopShedsOptionalPollsWhileStruggling(t *testing.T) {
	m, clock := slowModel(t)
	// Healthy: the light counters are polled.
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantTraffic(clock.Now()); !ok {
		t.Fatal("traffic must be polled while healthy")
	}
	m.trafficBusy = false

	clock.Advance(2 * time.Second)
	failFetch(t, m, clock, time.Second)
	// One failure: the cheap counters still flow, but nothing optional is added.
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantTraffic(clock.Now()); !ok {
		t.Fatal("the cheap traffic counters should keep flowing after one miss")
	}
	m.trafficBusy = false
	// A second miss in a row: shed everything but the heavy poll's retry.
	clock.Advance(m.backoff + 2*time.Second)
	failFetch(t, m, clock, time.Second)
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantTraffic(clock.Now()); ok {
		t.Fatal("two misses in a row: even the light poll must stop adding load")
	}
}

func TestTopSlowMarkerAndEventAreShownOnceAndCleared(t *testing.T) {
	m, clock := slowModel(t)
	slowEvents := func() int {
		n := 0
		for _, e := range m.events {
			if strings.Contains(e.Text, "provider slow") {
				n++
			}
		}
		return n
	}
	clock.Advance(2 * time.Second)
	failFetch(t, m, clock, 2*time.Second)
	clock.Advance(m.backoff + time.Second)
	failFetch(t, m, clock, 2*time.Second)
	clock.Advance(m.backoff + time.Second)
	failFetch(t, m, clock, 2*time.Second)
	if n := slowEvents(); n != 1 {
		t.Fatalf("%d slow events after three misses in one episode, want exactly 1", n)
	}
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "SLOW") {
		t.Fatalf("no SLOW marker on screen:\n%s", strings.Join(strings.Split(txt, "\n")[:3], "\n"))
	}
	if strings.Contains(txt, "DISCONNECTED") {
		t.Fatal("a slow provider is not disconnected")
	}

	clock.Advance(m.backoff + time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(50 * time.Millisecond)
	m.apply(gen, topFlowing(t), nil)
	txt, _ = showOnSim(t, m, 120, 40)
	if strings.Contains(txt, "SLOW") {
		t.Fatal("SLOW must clear when the provider answers promptly")
	}
	recovered := false
	for _, e := range m.events {
		if strings.Contains(e.Text, "responsive again") {
			recovered = true
		}
	}
	if !recovered {
		t.Fatal("no recovery event after a reported slow episode")
	}
}

// A blip that never got reported must not leave a recovery event behind either.
func TestTopBriefBlipLeavesNoEvents(t *testing.T) {
	m, clock := slowModel(t)
	before := len(m.events)
	clock.Advance(2 * time.Second)
	failFetch(t, m, clock, time.Second)
	clock.Advance(m.backoff + time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(50 * time.Millisecond)
	m.apply(gen, topFlowing(t), nil)
	if len(m.events) != before {
		t.Fatalf("a single missed answer left %d event(s): %+v", len(m.events)-before, m.events[before:])
	}
}

func TestTopLightCommandsUseAShortTimeout(t *testing.T) {
	if topLightTimeout >= 5*time.Second || topLightTimeout < 500*time.Millisecond {
		t.Fatalf("light timeout = %v, want well under the 5s heavy one and not absurdly short", topLightTimeout)
	}
}

// A snapshot applied with no recorded request start has no latency. It must not
// be read as an enormous one and mark a healthy node slow.
func TestTopSnapshotWithNoRecordedStartIsNotSlow(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	if m.isSlow() || m.backoff != 0 {
		t.Fatalf("a first snapshot marked the node slow (backoff %v)", m.backoff)
	}
	txt, _ := showOnSim(t, m, 120, 40)
	if strings.Contains(txt, "SLOW") {
		t.Fatal("SLOW shown for a healthy node")
	}
}

// Only a timeout means slow. A refused connection or a missing socket is what an
// update or a restart looks like, and that must still read as lost right away
// with its reason, however recently the provider answered.
func TestTopHardFailuresAreStillLostAtOnce(t *testing.T) {
	for _, msg := range []string{
		"live status unavailable: dial unix /home/u/.urnetwork/provider.sock: connect: connection refused",
		"live status unavailable: dial unix /home/u/.urnetwork/provider.sock: connect: no such file or directory",
	} {
		m, clock := slowModel(t)
		clock.Advance(time.Second)
		gen, _, _ := m.wantFetch(clock.Now())
		m.apply(gen, nil, errors.New(msg))
		if m.conn != topDisconnected {
			t.Errorf("%q: conn = %v, want lost at once", msg, m.conn)
		}
	}
}

func TestIsTimeoutErr(t *testing.T) {
	var real error = &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	wrapped := fmt.Errorf("%w: %w", errSnapshotUnavailable, real)
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"typed timeout survives the wrapping": {wrapped, true},
		"flattened text still counts":         {errors.New("read unix @->/x/provider.sock: i/o timeout"), true},
		"connection refused":                  {errors.New("connect: connection refused"), false},
		"missing socket":                      {fmt.Errorf("%w: %w", errSnapshotUnavailable, os.ErrNotExist), false},
		"nil":                                 {nil, false},
	} {
		if got := isTimeoutErr(tc.err); got != tc.want {
			t.Errorf("%s: isTimeoutErr = %v, want %v", name, got, tc.want)
		}
	}
}

// The spacing has to be measured from when a request FINISHED. From its start, a
// miss that took 5 seconds (the socket deadline) had already used up a 2 second
// backoff and the next request went out at once, which is the pile-on the backoff
// exists to stop.
func TestTopBackoffIsMeasuredFromCompletionNotStart(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	gen, _, ok := m.wantFetch(clock.Now())
	if !ok {
		t.Fatal("no fetch")
	}
	clock.Advance(5 * time.Second) // a full socket deadline
	m.apply(gen, nil, errSlow)
	if m.backoff == 0 {
		t.Fatal("no backoff after a miss")
	}
	// Right after the miss, and for interval+backoff after it, nothing is asked.
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("asked again the moment the 5s miss finished; the backoff spaced nothing")
	}
	clock.Advance(topSnapshotEvery + m.backoff - 100*time.Millisecond)
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("asked before interval+backoff had passed since the miss finished")
	}
	clock.Advance(200 * time.Millisecond)
	if _, _, ok := m.wantFetch(clock.Now()); !ok {
		t.Fatal("not asked once interval+backoff had passed")
	}
}

// A slow reply of T seconds spaces the next request by T after it arrived.
func TestTopSlowReplySpacesTheNextRequestFromItsArrival(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(4 * time.Second)
	m.apply(gen, topFlowing(t), nil)
	// 4s reply => backoff 4s. The next request is due interval+4s after arrival.
	clock.Advance(topSnapshotEvery + 3*time.Second)
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("re-polled while the provider was still digesting a 4s reply")
	}
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantFetch(clock.Now()); !ok {
		t.Fatal("never re-polled")
	}
}

// A healthy provider keeps its exact cadence: measured from the start, as before.
func TestTopHealthyCadenceIsUnchangedByTheCompletionRule(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(200 * time.Millisecond) // prompt reply
	m.apply(gen, topFlowing(t), nil)
	clock.Advance(topSnapshotEvery - 200*time.Millisecond) // 1s after the START of that request: due
	if _, _, ok := m.wantFetch(clock.Now()); !ok {
		t.Fatal("a healthy provider must be polled every interval measured from the start of the last request")
	}
}

// One deadline covers the whole exchange. The dial may use up to two seconds of
// it, and the request and reply get only what is left; setting the deadline after
// the dial gave them a second full budget.
func TestSocketRequestDeadlineIsFixedBeforeDialing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accepts, reads, never answers
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { buf := make([]byte, 512); c.Read(buf); time.Sleep(3 * time.Second); c.Close() }()
		}
	}()
	start := time.Now()
	_, err = sendSocketRequestTimeout(path, controlRequest{Cmd: "traffic"}, 400*time.Millisecond)
	took := time.Since(start)
	if err == nil {
		t.Fatal("a silent server must time out")
	}
	if took > 700*time.Millisecond {
		t.Fatalf("a 400ms budget took %v", took)
	}
	if got := socketDeadline(start, 400*time.Millisecond); !got.Equal(start.Add(400 * time.Millisecond)) {
		t.Fatalf("deadline = %v, want start+timeout", got)
	}
}

// The runtime views (Internals panel, goroutine list) are the first things
// dropped when the provider struggles: they are optional, and the goroutine
// profile is the most expensive thing top can ask for.
func TestTopShedsRuntimeViewsAtTheFirstSignOfStruggle(t *testing.T) {
	m, clock := slowModel(t)
	m.rt.showing = true // the goroutine list is on screen

	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantInternals(clock.Now()); !ok {
		t.Fatal("healthy: internals must be polled")
	}
	m.rt.busy = false
	if _, _, ok := m.wantGroups(clock.Now()); !ok {
		t.Fatal("healthy: the shown goroutine list must be polled")
	}
	m.rt.gBusy = false
	m.rt.gLast = time.Time{}

	failFetch(t, m, clock, time.Second) // one miss
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantInternals(clock.Now()); ok {
		t.Fatal("one miss: internals must stop being polled")
	}
	if _, _, ok := m.wantGroups(clock.Now()); ok {
		t.Fatal("one miss: the goroutine profile must stop being taken")
	}
	// The cheap traffic counters are still allowed after one miss.
	if _, _, ok := m.wantTraffic(clock.Now()); !ok {
		t.Fatal("one miss: the cheap traffic poll should still run")
	}

	// A prompt answer brings everything back.
	clock.Advance(m.backoff + time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(50 * time.Millisecond)
	m.apply(gen, topFlowing(t), nil)
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantInternals(clock.Now()); !ok {
		t.Fatal("recovered: internals must resume")
	}
}

// A reply that arrives but is slow also sheds them: backoff > 0 is the same
// signal as a miss.
func TestTopShedsRuntimeViewsWhileRepliesAreSlow(t *testing.T) {
	m, clock := slowModel(t)
	clock.Advance(2 * time.Second)
	gen, _, _ := m.wantFetch(clock.Now())
	clock.Advance(3 * time.Second)
	m.apply(gen, topFlowing(t), nil)
	clock.Advance(3 * time.Second)
	if _, _, ok := m.wantInternals(clock.Now()); ok {
		t.Fatal("slow replies: internals must not be polled")
	}
}

func TestTopRuntimeRequestDeadlines(t *testing.T) {
	if got := runtimeRequestTimeout("internals"); got != topLightTimeout {
		t.Errorf("internals deadline = %v, want the light %v", got, topLightTimeout)
	}
	if got := runtimeRequestTimeout("goroutines"); got <= topLightTimeout {
		t.Errorf("the goroutine profile is heavy and needs more than %v, got %v", topLightTimeout, got)
	}
}
