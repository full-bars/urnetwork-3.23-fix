package tcellui

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
)

// suspendCounter is a terminal that counts the suspend and resume calls top's
// adapter makes on it. It wraps a simulation screen, so the calls land on a
// real tcell screen.
type suspendCounter struct {
	tcell.Screen
	suspends, resumes int
	failResume        bool
}

func (c *suspendCounter) Suspend() error {
	c.suspends++
	return c.Screen.Suspend()
}

func (c *suspendCounter) Resume() error {
	c.resumes++
	if c.failResume {
		return errors.New("resume failed")
	}
	return c.Screen.Resume()
}

func TestScreenSuspendAndResumeReachTheTerminalOnce(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	sim.SetSize(40, 10)

	if err := scr.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if fake.suspends != 1 {
		t.Fatalf("terminal suspends = %d, want 1", fake.suspends)
	}
	// A second Suspend is not a second disengage: the terminal is already back
	// with the shell and tcell would refuse it.
	if err := scr.Suspend(); err != nil {
		t.Fatalf("the second Suspend: %v", err)
	}
	if fake.suspends != 1 {
		t.Fatalf("terminal suspends after a second call = %d, want 1", fake.suspends)
	}

	if err := scr.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if fake.resumes != 1 {
		t.Fatalf("terminal resumes = %d, want 1", fake.resumes)
	}
	// And a Resume with nothing suspended changes nothing.
	if err := scr.Resume(); err != nil {
		t.Fatalf("the second Resume: %v", err)
	}
	if fake.resumes != 1 {
		t.Fatalf("terminal resumes after a second call = %d, want 1", fake.resumes)
	}

	// The screen draws again once it is back.
	b := tui.New(20, 3)
	b.Put("back", 0, 0, tui.Style{})
	scr.Draw(b)
}

func TestScreenDrawIsHeldWhileSuspended(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	sim.SetSize(20, 3)

	if err := scr.Suspend(); err != nil {
		t.Fatal(err)
	}
	b := tui.New(20, 3)
	b.Put("suspend", 0, 0, tui.Style{})
	scr.Draw(b)
	// The frame must not reach the terminal while the shell (or sudo) owns it.
	if got := simText(sim); got != "" {
		t.Fatalf("a draw while suspended reached the terminal:\n%q", got)
	}
}

func TestScreenResumeReportsAFailedResume(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim, failResume: true}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	if err := scr.Suspend(); err != nil {
		t.Fatal(err)
	}
	if err := scr.Resume(); err == nil {
		t.Fatal("a failed resume should report the error")
	}
	// The terminal never came back, so the screen stays suspended and a frame
	// must not reach it.
	b := tui.New(10, 2)
	b.Put("x", 0, 0, tui.Style{})
	scr.Draw(b)
	if got := simText(sim); got != "" {
		t.Fatalf("a draw reached a terminal that did not come back:\n%q", got)
	}
	// A later Resume tries again, and the frame goes out once it succeeds.
	fake.failResume = false
	if err := scr.Resume(); err != nil {
		t.Fatalf("the second Resume: %v", err)
	}
	if fake.resumes != 2 {
		t.Fatalf("terminal resumes = %d, want 2", fake.resumes)
	}
	scr.Draw(b)
	if got := simText(sim); !strings.Contains(got, "x") {
		t.Fatalf("no frame after the terminal came back:\n%q", got)
	}
}

// simText reads the simulated terminal as text, one line per row.
func simText(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	out := make([]rune, 0, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				out = append(out, ' ')
				continue
			}
			out = append(out, c.Runes[0])
		}
	}
	blank := true
	for _, r := range out {
		if r != ' ' {
			blank = false
			break
		}
	}
	if blank {
		return ""
	}
	return string(out)
}

// queueTerminal models an input producer that cannot finish until PollEvent
// drains its bounded queue.
type queueTerminal struct {
	tcell.Screen
	q            chan tcell.Event
	polled       chan struct{}
	producerDone chan struct{}
}

func (f *queueTerminal) PollEvent() tcell.Event {
	ev := <-f.q
	select {
	case f.polled <- struct{}{}:
	default:
	}
	return ev
}
func (f *queueTerminal) PostEventWait(ev tcell.Event) { f.q <- ev }
func (f *queueTerminal) Suspend() error               { <-f.producerDone; return nil }
func (f *queueTerminal) Resume() error                { return nil }
func (f *queueTerminal) Fini()                        { f.q <- nil; f.Screen.Fini() }

func TestSuspendDrainsInputAndDiscardsQueuedEvents(t *testing.T) {
	f := &queueTerminal{Screen: tcell.NewSimulationScreen("UTF-8"), q: make(chan tcell.Event, 2), polled: make(chan struct{}, 32), producerDone: make(chan struct{})}
	scr, err := Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	go func() {
		for i := 0; i < 25; i++ {
			f.q <- tcell.NewEventKey(tcell.KeyRune, 'x', 0)
		}
		close(f.producerDone)
	}()
	for i := 0; i < 17; i++ {
		<-f.polled
	}
	if err := scr.Suspend(); err != nil {
		t.Fatal(err)
	}
	if err := scr.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-scr.Events():
		t.Fatalf("queued event survived handoff: %+v", ev)
	default:
	}
}

// burstyTty feeds chunks from a buffered channel so tests can saturate tcell's
// keychan (capacity 10) without assuming a fixed chunk count.
type burstyTty struct {
	reads  chan []byte
	mu     sync.Mutex
	stop   chan struct{}
	closed bool
}

func newBurstyTty() *burstyTty {
	return &burstyTty{
		reads: make(chan []byte, 64),
		stop:  make(chan struct{}),
	}
}

func (b *burstyTty) Start() error {
	b.mu.Lock()
	b.stop = make(chan struct{})
	b.closed = false
	b.mu.Unlock()
	return nil
}

func (b *burstyTty) stopReads() {
	b.mu.Lock()
	if !b.closed {
		close(b.stop)
		b.closed = true
	}
	b.mu.Unlock()
}

func (b *burstyTty) Stop() error {
	b.stopReads()
	return nil
}

func (b *burstyTty) Drain() error {
	b.stopReads()
	return nil
}

func (b *burstyTty) clearPending() {
	for {
		select {
		case <-b.reads:
		default:
			return
		}
	}
}

func (b *burstyTty) Read(p []byte) (int, error) {
	b.mu.Lock()
	stop := b.stop
	b.mu.Unlock()
	select {
	case chunk := <-b.reads:
		return copy(p, chunk), nil
	case <-stop:
		return 0, io.EOF
	}
}

func (b *burstyTty) Write(p []byte) (int, error) { return len(p), nil }
func (b *burstyTty) Close() error                { return b.Stop() }
func (b *burstyTty) NotifyResize(func())         {}
func (b *burstyTty) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: 80, Height: 24}, nil
}

// waitBurstySaturated waits until tty.reads still holds unconsumed chunks and
// its length stops falling, which means inputLoop is blocked sending to
// tcell's keychan rather than reading from tty.
func waitBurstySaturated(t *testing.T, tty *burstyTty, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	prev := len(tty.reads)
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		cur := len(tty.reads)
		if cur > 0 && cur == prev {
			stable++
			if stable >= 3 {
				return
			}
		} else {
			stable = 0
			prev = cur
		}
	}
	t.Fatalf("timed out waiting for tty reads to saturate keychan (pending=%d)", len(tty.reads))
}

func TestSuspendDoesNotDeadlockWhenTcellKeychanFills(t *testing.T) {
	ti, err := tcell.LookupTerminfo("xterm")
	if err != nil {
		t.Fatal(err)
	}
	tty := newBurstyTty()
	ts, err := tcell.NewTerminfoScreenFromTtyTerminfo(wrapSuspendTty(tty), ti)
	if err != nil {
		t.Fatal(err)
	}
	scr, err := Wrap(ts)
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("x", 128))
	for i := 0; i < 24; i++ {
		tty.reads <- chunk
	}
	waitBurstySaturated(t, tty, 2*time.Second)

	done := make(chan error, 1)
	go func() {
		done <- scr.Suspend()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Suspend failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Suspend deadlocked with full keychan")
	}

	// Discard unread chunks still sitting in the fake tty before Resume so any
	// 'x' event seen after Resume could only come from leftovers in tcell's
	// internal queues.
	tty.clearPending()
	if err := scr.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	tty.reads <- []byte("y")
	select {
	case ev := <-scr.Events():
		if ev.Kind != EventRune || ev.Rune != 'y' {
			t.Fatalf("pre-suspend event survived handoff: got %+v, want rune 'y'", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-resume input")
	}

	// Saturate keychan again and verify Close (Fini) also completes without
	// deadlocking on keychan.
	for i := 0; i < 24; i++ {
		tty.reads <- chunk
	}
	waitBurstySaturated(t, tty, 2*time.Second)

	closed := make(chan struct{})
	go func() {
		scr.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked with full keychan")
	}
}

func TestSuspendWhileIdleInReadAfterPartialEscape(t *testing.T) {
	ti, err := tcell.LookupTerminfo("xterm")
	if err != nil {
		t.Fatal(err)
	}
	tty := newBurstyTty()
	ts, err := tcell.NewTerminfoScreenFromTtyTerminfo(wrapSuspendTty(tty), ti)
	if err != nil {
		t.Fatal(err)
	}
	scr, err := Wrap(ts)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()

	// Leave inputProcessor mid-OSC and inputLoop blocked inside tty.Read.
	tty.reads <- []byte("\x1b]0;partial-title")
	deadline := time.Now().Add(2 * time.Second)
	for len(tty.reads) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	done := make(chan error, 1)
	go func() { done <- scr.Suspend() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Suspend failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Suspend deadlocked while idle in Read after partial escape")
	}

	if err := scr.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	tty.reads <- []byte("y")
	select {
	case ev := <-scr.Events():
		if ev.Kind != EventRune || ev.Rune != 'y' {
			t.Fatalf("post-resume event = %+v, want rune 'y'", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-resume input")
	}
}
